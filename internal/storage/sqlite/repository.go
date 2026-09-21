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
	result, err := db.db.ExecContext(ctx, "DELETE FROM content_blobs WHERE created_at < ?", now.Add(-retention).UnixNano())
	if err != nil {
		return 0, failure("delete expired content", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return 0, failure("count expired content", err)
	}
	return count, nil
}
