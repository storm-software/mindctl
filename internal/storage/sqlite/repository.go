package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"sync"
	"time"

	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage"
)

type transaction struct {
	mu      sync.Mutex
	ctx     context.Context
	conn    *sql.Conn
	keyring *contentcrypto.Keyring
	done    bool
	err     error
}

func (db *DB) WithTx(ctx context.Context, fn func(storage.Tx) error) error {
	return inTransaction(ctx, db.db, true, func(conn *sql.Conn) error {
		tx := &transaction{ctx: ctx, conn: conn, keyring: db.keyring}
		defer func() {
			tx.mu.Lock()
			tx.done = true
			tx.mu.Unlock()
		}()
		if err := fn(tx); err != nil {
			return err
		}
		tx.mu.Lock()
		defer tx.mu.Unlock()
		return tx.err
	})
}

func (tx *transaction) InsertRequest(record storage.RequestRecord) error {
	tx.mu.Lock()
	defer tx.mu.Unlock()
	if tx.done {
		return failure("insert after transaction", sql.ErrTxDone)
	}
	if tx.err == nil {
		tx.err = tx.insertRequest(record)
	}
	return tx.err
}

type encryptedContent struct {
	kind     string
	position int
	envelope contentcrypto.Envelope
}

func (tx *transaction) insertRequest(record storage.RequestRecord) error {
	if record.ID == "" {
		return failure("validate request", errors.New("request ID is required"))
	}
	if record.CreatedAt.IsZero() {
		record.CreatedAt = time.Now().UTC()
	}
	// Encrypt every captured value before executing any SQL for this record.
	var contents []encryptedContent
	encrypt := func(kind string, position int, content []byte) error {
		if content == nil {
			return nil
		}
		envelope, err := tx.keyring.Encrypt(content)
		if err != nil {
			return failure("encrypt content", err)
		}
		contents = append(contents, encryptedContent{kind, position, envelope})
		return nil
	}
	for _, item := range []struct {
		kind string
		data []byte
	}{{"prompt", record.Prompt}, {"answer", record.Answer}, {"raw", record.RawContent}} {
		if err := encrypt(item.kind, 0, item.data); err != nil {
			return err
		}
	}
	for index, content := range record.RejectedOutputs {
		if err := encrypt("rejected", index, content); err != nil {
			return err
		}
	}
	// These types contain only routing metadata, never raw captured fields.
	reasons, err := json.Marshal(record.Decision.Reasons)
	if err != nil {
		return failure("encode decision reasons", err)
	}
	rejections, err := json.Marshal(record.Decision.Rejections)
	if err != nil {
		return failure("encode decision rejections", err)
	}
	replay, err := json.Marshal(record.Replay)
	if err != nil {
		return failure("encode replay snapshot", err)
	}
	judgment, err := json.Marshal(record.Judgment)
	if err != nil {
		return failure("encode judgment", err)
	}
	createdAt := record.CreatedAt.UnixNano()
	if _, err := tx.conn.ExecContext(tx.ctx, "INSERT INTO requests (id, created_at) VALUES (?, ?)", record.ID, createdAt); err != nil {
		return failure("insert request", err)
	}
	if _, err := tx.conn.ExecContext(tx.ctx, `INSERT INTO routing_decisions
		(request_id, tier, model_id, provider, reasons_json, rejections_json, replay_json) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		record.ID, record.Decision.Tier, record.Decision.ModelID, record.Decision.Provider, string(reasons), string(rejections), string(replay)); err != nil {
		return failure("insert decision", err)
	}
	for position, score := range record.Decision.Candidates {
		if _, err := tx.conn.ExecContext(tx.ctx, `INSERT INTO candidate_scores
			(request_id, position, model_id, provider, direct_cost, failure_probability, escalation_cost,
			success_probability, latency_penalty, expected_total_cost, latency_ns) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			record.ID, position, score.ModelID, score.Provider, score.DirectCost, score.FailureProbability, score.EscalationCost,
			score.SuccessProbability, score.LatencyPenalty, score.ExpectedTotalCost, int64(score.Latency)); err != nil {
			return failure("insert candidate score", err)
		}
	}
	if record.Judgment != nil {
		if _, err := tx.conn.ExecContext(tx.ctx, "INSERT INTO jev_judgments (request_id, judgment_json) VALUES (?, ?)", record.ID, string(judgment)); err != nil {
			return failure("insert judgment", err)
		}
	}
	for _, content := range contents {
		envelope := content.envelope
		if _, err := tx.conn.ExecContext(tx.ctx, `INSERT INTO content_blobs
			(request_id, kind, position, key_id, version, nonce, ciphertext, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			record.ID, content.kind, content.position, envelope.KeyID, envelope.Version, envelope.Nonce, envelope.Ciphertext, createdAt); err != nil {
			return failure("insert encrypted content", err)
		}
	}
	return nil
}

func (db *DB) GetRequest(ctx context.Context, id string) (storage.RequestRecord, error) {
	var record storage.RequestRecord
	err := inTransaction(ctx, db.db, false, func(conn *sql.Conn) error {
		var createdAt int64
		var reasons, rejections, replay string
		err := conn.QueryRowContext(ctx, `SELECT r.id, r.created_at, d.tier, d.model_id, d.provider,
			d.reasons_json, d.rejections_json, d.replay_json FROM requests r
			JOIN routing_decisions d ON d.request_id = r.id WHERE r.id = ?`, id).Scan(
			&record.ID, &createdAt, &record.Decision.Tier, &record.Decision.ModelID, &record.Decision.Provider,
			&reasons, &rejections, &replay)
		if errors.Is(err, sql.ErrNoRows) {
			return storage.ErrNotFound
		}
		if err != nil {
			return failure("read request", err)
		}
		record.CreatedAt = time.Unix(0, createdAt).UTC()
		for _, item := range []struct {
			data string
			into any
		}{{reasons, &record.Decision.Reasons}, {rejections, &record.Decision.Rejections}, {replay, &record.Replay}} {
			if err := json.Unmarshal([]byte(item.data), item.into); err != nil {
				return failure("decode decision", err)
			}
		}
		if err := readCandidates(ctx, conn, &record); err != nil {
			return err
		}
		var judgment string
		err = conn.QueryRowContext(ctx, "SELECT judgment_json FROM jev_judgments WHERE request_id = ?", id).Scan(&judgment)
		if err == nil {
			record.Judgment = new(domain.JevJudgment)
			if err := json.Unmarshal([]byte(judgment), record.Judgment); err != nil {
				return failure("decode judgment", err)
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return failure("read judgment", err)
		}
		return db.readContent(ctx, conn, &record)
	})
	if err != nil {
		// Do not expose partially decrypted content after authentication failure.
		return storage.RequestRecord{}, err
	}
	return record, nil
}

func readCandidates(ctx context.Context, conn *sql.Conn, record *storage.RequestRecord) error {
	rows, err := conn.QueryContext(ctx, `SELECT model_id, provider, direct_cost, failure_probability,
		escalation_cost, success_probability, latency_penalty, expected_total_cost, latency_ns
		FROM candidate_scores WHERE request_id = ? ORDER BY position`, record.ID)
	if err != nil {
		return failure("read candidate scores", err)
	}
	defer rows.Close()
	for rows.Next() {
		var score router.CandidateScore
		if err := rows.Scan(&score.ModelID, &score.Provider, &score.DirectCost, &score.FailureProbability,
			&score.EscalationCost, &score.SuccessProbability, &score.LatencyPenalty, &score.ExpectedTotalCost, &score.Latency); err != nil {
			return failure("decode candidate score", err)
		}
		record.Decision.Candidates = append(record.Decision.Candidates, score)
	}
	if err := rows.Err(); err != nil {
		return failure("iterate candidate scores", err)
	}
	return nil
}

func (db *DB) readContent(ctx context.Context, conn *sql.Conn, record *storage.RequestRecord) error {
	rows, err := conn.QueryContext(ctx, `SELECT kind, position, key_id, version, nonce, ciphertext
		FROM content_blobs WHERE request_id = ? ORDER BY kind, position`, record.ID)
	if err != nil {
		return failure("read content", err)
	}
	defer rows.Close()
	for rows.Next() {
		var kind string
		var position int
		var envelope contentcrypto.Envelope
		if err := rows.Scan(&kind, &position, &envelope.KeyID, &envelope.Version, &envelope.Nonce, &envelope.Ciphertext); err != nil {
			return failure("decode content envelope", err)
		}
		content, err := db.keyring.Decrypt(envelope)
		if err != nil {
			return failure("decrypt content", err)
		}
		switch kind {
		case "prompt":
			record.Prompt = content
		case "answer":
			record.Answer = content
		case "raw":
			record.RawContent = content
		case "rejected":
			for len(record.RejectedOutputs) <= position {
				record.RejectedOutputs = append(record.RejectedOutputs, nil)
			}
			record.RejectedOutputs[position] = content
		}
	}
	if err := rows.Err(); err != nil {
		return failure("iterate content", err)
	}
	return nil
}

func (db *DB) DeleteExpiredContent(ctx context.Context, retention time.Duration, now time.Time) (int64, error) {
	if retention == 0 {
		return 0, nil
	}
	if retention < 0 {
		return 0, failure("validate retention", errors.New("retention must be nonnegative"))
	}
	cutoff := now.Add(-retention).UnixNano()
	var count int64
	err := inTransaction(ctx, db.db, true, func(conn *sql.Conn) error {
		for _, statement := range []string{
			"DELETE FROM content_blobs WHERE created_at < ?",
			"DELETE FROM transcript_items WHERE created_at < ?",
			`UPDATE provider_attempts SET key_id = NULL, version = NULL, nonce = NULL, ciphertext = NULL
				WHERE created_at < ? AND ciphertext IS NOT NULL`,
		} {
			result, err := conn.ExecContext(ctx, statement, cutoff)
			if err != nil {
				return failure("delete expired content", err)
			}
			removed, err := result.RowsAffected()
			if err != nil {
				return failure("count expired content", err)
			}
			count += removed
		}
		return nil
	})
	if err != nil {
		return 0, err
	}
	return count, nil
}

func (db *DB) CreateTurn(ctx context.Context, turn storage.NewTurn) error {
	return inTransaction(ctx, db.db, true, func(conn *sql.Conn) error {
		if turn.Conversation.ID == "" || turn.Conversation.ClientID == "" || turn.Response.ID == "" {
			return failure("validate conversation turn", errors.New("conversation, client, and response IDs are required"))
		}
		created := turn.Response.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		conversationCreated := turn.Conversation.CreatedAt
		if conversationCreated.IsZero() {
			conversationCreated = created
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO conversations
			(id, client_id, created_at, escalation_floor) VALUES (?, ?, ?, ?)
			ON CONFLICT(id) DO NOTHING`, turn.Conversation.ID, turn.Conversation.ClientID, conversationCreated.UnixNano(), turn.Conversation.Floor); err != nil {
			return failure("insert conversation", err)
		}
		var owner string
		if err := conn.QueryRowContext(ctx, "SELECT client_id FROM conversations WHERE id = ?", turn.Conversation.ID).Scan(&owner); err != nil {
			return failure("read conversation owner", err)
		}
		if owner != turn.Conversation.ClientID {
			return storage.ErrNotFound
		}
		var sequence int
		if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence) + 1, 0) FROM responses WHERE conversation_id = ?", turn.Conversation.ID).Scan(&sequence); err != nil {
			return failure("allocate response sequence", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO responses (id, conversation_id, sequence, created_at, status)
			VALUES (?, ?, ?, ?, 'pending')`, turn.Response.ID, turn.Conversation.ID, sequence, created.UnixNano()); err != nil {
			return failure("insert response", err)
		}
		return insertTranscriptItems(ctx, conn, db.keyring, turn.Response.ID, "", turn.Input, created)
	})
}

func (db *DB) GetConversationTurn(ctx context.Context, clientID, responseID string) (storage.ConversationTurn, error) {
	var turn storage.ConversationTurn
	err := inTransaction(ctx, db.db, false, func(conn *sql.Conn) error {
		var conversationCreated, responseCreated int64
		var provider, model sql.NullString
		var pinTier sql.NullInt64
		if err := conn.QueryRowContext(ctx, `SELECT c.id, c.client_id, c.created_at, c.pin_provider, c.pin_model_id,
			c.pin_tier, c.escalation_floor, r.id, r.conversation_id, r.sequence, r.created_at, r.status
			FROM responses r JOIN conversations c ON c.id = r.conversation_id
			WHERE c.client_id = ? AND r.id = ?`, clientID, responseID).Scan(
			&turn.Conversation.ID, &turn.Conversation.ClientID, &conversationCreated, &provider, &model, &pinTier, &turn.Conversation.Floor,
			&turn.Response.ID, &turn.Response.ConversationID, &turn.Response.Sequence, &responseCreated, &turn.Response.Status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storage.ErrNotFound
			}
			return failure("read conversation response", err)
		}
		turn.Conversation.CreatedAt = time.Unix(0, conversationCreated).UTC()
		turn.Response.CreatedAt = time.Unix(0, responseCreated).UTC()
		if provider.Valid || model.Valid || pinTier.Valid {
			if !provider.Valid || !model.Valid || !pinTier.Valid {
				return failure("read conversation pin", errors.New("incomplete stored pin"))
			}
			turn.Conversation.Pin = &router.Pin{Provider: provider.String, ModelID: model.String, Floor: domain.Tier(pinTier.Int64)}
		}
		return db.readTranscript(ctx, conn, &turn)
	})
	if err != nil {
		return storage.ConversationTurn{}, err
	}
	return turn, nil
}

func (db *DB) readTranscript(ctx context.Context, conn *sql.Conn, turn *storage.ConversationTurn) error {
	rows, err := conn.QueryContext(ctx, `SELECT i.response_id, i.position, i.provider, i.key_id, i.version, i.nonce, i.ciphertext
		FROM transcript_items i JOIN responses r ON r.id = i.response_id
		WHERE r.conversation_id = ? AND r.sequence <= ? ORDER BY r.sequence, i.position`, turn.Conversation.ID, turn.Response.Sequence)
	if err != nil {
		return failure("read transcript", err)
	}
	defer rows.Close()
	for rows.Next() {
		var entry storage.TranscriptItem
		var envelope contentcrypto.Envelope
		if err := rows.Scan(&entry.ResponseID, &entry.Position, &entry.Provider, &envelope.KeyID, &envelope.Version, &envelope.Nonce, &envelope.Ciphertext); err != nil {
			return failure("decode transcript envelope", err)
		}
		plain, err := db.keyring.Decrypt(envelope)
		if err != nil {
			return failure("decrypt transcript", err)
		}
		if err := json.Unmarshal(plain, &entry.Item); err != nil {
			return failure("decode transcript item", err)
		}
		turn.Transcript = append(turn.Transcript, entry)
	}
	if err := rows.Err(); err != nil {
		return failure("iterate transcript", err)
	}
	return nil
}

func (db *DB) BeginProviderAttempt(ctx context.Context, attempt storage.ProviderAttempt) error {
	return inTransaction(ctx, db.db, true, func(conn *sql.Conn) error {
		if attempt.ID == "" || attempt.ClientID == "" || attempt.ResponseID == "" {
			return failure("validate provider attempt", errors.New("attempt, client, and response IDs are required"))
		}
		var responseStatus, conversationID string
		var pinProvider, pinModel sql.NullString
		var pinTier sql.NullInt64
		var floor domain.Tier
		if err := conn.QueryRowContext(ctx, `SELECT r.status, c.id, c.pin_provider, c.pin_model_id, c.pin_tier, c.escalation_floor
			FROM responses r JOIN conversations c ON c.id = r.conversation_id WHERE c.client_id = ? AND r.id = ?`, attempt.ClientID, attempt.ResponseID).Scan(
			&responseStatus, &conversationID, &pinProvider, &pinModel, &pinTier, &floor); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storage.ErrNotFound
			}
			return failure("read attempt response", err)
		}
		if responseStatus != "pending" {
			return failure("begin provider attempt", errors.New("response is already completed"))
		}
		if attempt.Decision.Provider == "" || attempt.Decision.ModelID == "" || !attempt.Decision.Tier.Valid() {
			return failure("validate provider decision", errors.New("provider, model, and valid tier are required"))
		}
		if pinProvider.Valid || pinModel.Valid || pinTier.Valid {
			if !pinProvider.Valid || !pinModel.Valid || !pinTier.Valid {
				return failure("read conversation pin", errors.New("incomplete stored pin"))
			}
			if attempt.Decision.Tier < domain.Tier(pinTier.Int64) || attempt.Decision.Tier < floor {
				return failure("validate provider decision", errors.New("decision tier would lower the conversation pin or floor"))
			}
		} else {
			if attempt.Decision.Tier < floor {
				return failure("validate provider decision", errors.New("decision tier would lower the conversation floor"))
			}
			if attempt.Decision.Tier > floor {
				floor = attempt.Decision.Tier
			}
			if _, err := conn.ExecContext(ctx, `UPDATE conversations SET pin_provider = ?, pin_model_id = ?, pin_tier = ?, escalation_floor = ? WHERE id = ?`,
				attempt.Decision.Provider, attempt.Decision.ModelID, attempt.Decision.Tier, floor, conversationID); err != nil {
				return failure("persist initial conversation pin", err)
			}
		}
		decision, err := json.Marshal(attempt.Decision)
		if err != nil {
			return failure("encode provider decision", err)
		}
		var sequence int
		if err := conn.QueryRowContext(ctx, "SELECT COALESCE(MAX(sequence) + 1, 0) FROM provider_attempts WHERE response_id = ?", attempt.ResponseID).Scan(&sequence); err != nil {
			return failure("allocate attempt sequence", err)
		}
		created := attempt.CreatedAt
		if created.IsZero() {
			created = time.Now().UTC()
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO provider_attempts
			(id, response_id, sequence, provider, model_id, tier, decision_json, status, created_at)
			VALUES (?, ?, ?, ?, ?, ?, ?, 'started', ?)`, attempt.ID, attempt.ResponseID, sequence, attempt.Decision.Provider,
			attempt.Decision.ModelID, attempt.Decision.Tier, string(decision), created.UnixNano()); err != nil {
			return failure("insert provider attempt", err)
		}
		return nil
	})
}

func (db *DB) CommitConversationResult(ctx context.Context, clientID, responseID string, pin router.Pin, result inference.Result) error {
	return inTransaction(ctx, db.db, true, func(conn *sql.Conn) error {
		if !pin.Floor.Valid() || pin.Provider == "" || pin.ModelID == "" {
			return failure("validate conversation pin", errors.New("provider, model, and valid tier are required"))
		}
		var conversationID, status string
		var currentProvider, currentModel sql.NullString
		var currentTier sql.NullInt64
		var floor domain.Tier
		if err := conn.QueryRowContext(ctx, `SELECT c.id, r.status FROM responses r JOIN conversations c ON c.id = r.conversation_id
			WHERE c.client_id = ? AND r.id = ?`, clientID, responseID).Scan(&conversationID, &status); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storage.ErrNotFound
			}
			return failure("read commit response", err)
		}
		if status != "pending" {
			return failure("commit response", errors.New("response is already completed"))
		}
		var attemptID, provider, model string
		var tier domain.Tier
		if err := conn.QueryRowContext(ctx, `SELECT id, provider, model_id, tier FROM provider_attempts
			WHERE response_id = ? AND status = 'started' ORDER BY sequence DESC LIMIT 1`, responseID).Scan(&attemptID, &provider, &model, &tier); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return failure("commit response", errors.New("no started provider attempt"))
			}
			return failure("read started provider attempt", err)
		}
		if pin.Provider != provider || pin.ModelID != model || pin.Floor != tier {
			return failure("validate result pin", errors.New("result pin does not match started provider attempt"))
		}
		if err := conn.QueryRowContext(ctx, `SELECT pin_provider, pin_model_id, pin_tier, escalation_floor FROM conversations WHERE id = ?`, conversationID).Scan(
			&currentProvider, &currentModel, &currentTier, &floor); err != nil {
			return failure("read conversation pin", err)
		}
		if !currentProvider.Valid || !currentModel.Valid || !currentTier.Valid {
			return failure("read conversation pin", errors.New("missing stored pin"))
		}
		if tier < domain.Tier(currentTier.Int64) || tier < floor {
			return failure("validate result pin", errors.New("result tier would lower the conversation pin or floor"))
		}
		if tier > floor {
			floor = tier
		}
		body, err := json.Marshal(result)
		if err != nil {
			return failure("encode provider result", err)
		}
		now := time.Now().UTC()
		if err := insertTranscriptItems(ctx, conn, db.keyring, responseID, pin.Provider, result.Output, now); err != nil {
			return err
		}
		if _, err := conn.ExecContext(ctx, `UPDATE conversations SET pin_provider = ?, pin_model_id = ?, pin_tier = ?, escalation_floor = ? WHERE id = ?`,
			pin.Provider, pin.ModelID, tier, floor, conversationID); err != nil {
			return failure("update conversation pin", err)
		}
		if _, err := conn.ExecContext(ctx, "UPDATE responses SET status = 'completed' WHERE id = ?", responseID); err != nil {
			return failure("complete response", err)
		}
		if err := completeAttempt(ctx, conn, db.keyring, attemptID, "succeeded", result.ProviderRequestID, body, now); err != nil {
			return err
		}
		return nil
	})
}

func (db *DB) FailProviderAttempt(ctx context.Context, clientID, responseID, attemptID, providerRequestID string, body []byte) error {
	return inTransaction(ctx, db.db, true, func(conn *sql.Conn) error {
		var count int
		if err := conn.QueryRowContext(ctx, `SELECT count(*) FROM provider_attempts a JOIN responses r ON r.id = a.response_id
			JOIN conversations c ON c.id = r.conversation_id WHERE c.client_id = ? AND r.id = ? AND a.id = ? AND a.status = 'started'`, clientID, responseID, attemptID).Scan(&count); err != nil {
			return failure("read provider attempt", err)
		}
		if count == 0 {
			return storage.ErrNotFound
		}
		return completeAttempt(ctx, conn, db.keyring, attemptID, "failed", providerRequestID, body, time.Now().UTC())
	})
}

// RaiseConversationFloor records a post-emission streaming failure without
// changing the existing pin or lowering an already stronger floor.
func (db *DB) RaiseConversationFloor(ctx context.Context, clientID, responseID string, floor domain.Tier) error {
	return inTransaction(ctx, db.db, true, func(conn *sql.Conn) error {
		if !floor.Valid() {
			return failure("validate conversation floor", errors.New("floor is invalid"))
		}
		var conversationID string
		var current domain.Tier
		if err := conn.QueryRowContext(ctx, `SELECT c.id, c.escalation_floor FROM responses r
			JOIN conversations c ON c.id = r.conversation_id WHERE c.client_id = ? AND r.id = ?`, clientID, responseID).Scan(&conversationID, &current); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return storage.ErrNotFound
			}
			return failure("read conversation floor", err)
		}
		if floor <= current {
			return nil
		}
		if _, err := conn.ExecContext(ctx, "UPDATE conversations SET escalation_floor = ? WHERE id = ?", floor, conversationID); err != nil {
			return failure("raise conversation floor", err)
		}
		return nil
	})
}

func insertTranscriptItems(ctx context.Context, conn *sql.Conn, keyring *contentcrypto.Keyring, responseID, provider string, items []inference.Item, created time.Time) error {
	var offset int
	if err := conn.QueryRowContext(ctx, "SELECT count(*) FROM transcript_items WHERE response_id = ?", responseID).Scan(&offset); err != nil {
		return failure("count transcript items", err)
	}
	for position, item := range items {
		body, err := json.Marshal(item)
		if err != nil {
			return failure("encode transcript item", err)
		}
		envelope, err := keyring.Encrypt(body)
		if err != nil {
			return failure("encrypt transcript", err)
		}
		if _, err := conn.ExecContext(ctx, `INSERT INTO transcript_items
			(response_id, position, provider, key_id, version, nonce, ciphertext, created_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
			responseID, offset+position, provider, envelope.KeyID, envelope.Version, envelope.Nonce, envelope.Ciphertext, created.UnixNano()); err != nil {
			return failure("insert transcript item", err)
		}
	}
	return nil
}

func completeAttempt(ctx context.Context, conn *sql.Conn, keyring *contentcrypto.Keyring, attemptID, status, providerRequestID string, body []byte, completed time.Time) error {
	envelope, err := keyring.Encrypt(body)
	if err != nil {
		return failure("encrypt provider body", err)
	}
	result, err := conn.ExecContext(ctx, `UPDATE provider_attempts SET status = ?, provider_request_id = ?, key_id = ?, version = ?, nonce = ?, ciphertext = ?, completed_at = ?
		WHERE id = ? AND status = 'started'`, status, providerRequestID, envelope.KeyID, envelope.Version, envelope.Nonce, envelope.Ciphertext, completed.UnixNano(), attemptID)
	if err != nil {
		return failure("complete provider attempt", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return failure("count completed provider attempt", err)
	}
	if count != 1 {
		return storage.ErrNotFound
	}
	return nil
}
