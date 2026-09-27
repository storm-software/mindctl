package sqlite

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage"
)

type sessionHarness struct {
	t   *testing.T
	db  *DB
	ctx context.Context
	ttl time.Duration
	seq int
}

func newSessionHarness(t *testing.T) *sessionHarness {
	t.Helper()
	return &sessionHarness{t: t, db: openTestDB(t, filepath.Join(t.TempDir(), "sessions.db")), ctx: context.Background(), ttl: time.Hour}
}

// start creates a turn in a fresh conversation bound to key and returns its
// response ID and the seeded conversation turn.
func (h *sessionHarness) start(clientID, key string, at time.Time, input ...inference.Item) (string, storage.ConversationTurn) {
	h.t.Helper()
	h.seq++
	suffix := string(rune('a' + h.seq))
	conversationID, responseID := "conv_"+suffix, "resp_"+suffix
	if len(input) == 0 {
		input = []inference.Item{{Type: "message", Role: "user", Text: "hi"}}
	}
	if err := h.db.CreateTurn(h.ctx, storage.NewTurn{
		Conversation: storage.ConversationRecord{ID: conversationID, ClientID: clientID, CreatedAt: at},
		Response:     storage.ResponseRecord{ID: responseID, ConversationID: conversationID, CreatedAt: at},
		Input:        input,
		SessionKey:   key, SessionIdleTTL: h.ttl,
	}); err != nil {
		h.t.Fatalf("CreateTurn() error = %v", err)
	}
	turn, err := h.db.GetConversationTurn(h.ctx, clientID, responseID)
	if err != nil {
		h.t.Fatalf("GetConversationTurn() error = %v", err)
	}
	return responseID, turn
}

func (h *sessionHarness) begin(clientID, responseID string, pin router.Pin) error {
	h.seq++
	return h.db.BeginProviderAttempt(h.ctx, storage.ProviderAttempt{
		ID: "att_" + string(rune('a'+h.seq)), ClientID: clientID, ResponseID: responseID,
		Decision: router.Decision{Provider: pin.Provider, ModelID: pin.ModelID, Tier: pin.Floor},
	})
}

func (h *sessionHarness) serve(clientID, responseID string, pin router.Pin, usage inference.Usage) {
	h.t.Helper()
	if err := h.begin(clientID, responseID, pin); err != nil {
		h.t.Fatalf("BeginProviderAttempt() error = %v", err)
	}
	if err := h.db.CommitConversationResult(h.ctx, clientID, responseID, pin, inference.Result{
		Status: "completed", Usage: usage,
		Output: []inference.Item{{Type: "message", Role: "assistant", Text: "hello"}},
	}); err != nil {
		h.t.Fatalf("CommitConversationResult() error = %v", err)
	}
}

func assertSeed(t *testing.T, turn storage.ConversationTurn, pin *router.Pin, floor domain.Tier) {
	t.Helper()
	got := turn.Conversation.Pin
	if (got == nil) != (pin == nil) || (got != nil && *got != *pin) || turn.Conversation.Floor != floor {
		t.Fatalf("seeded pin=%+v floor=%s; want pin=%+v floor=%s", got, turn.Conversation.Floor, pin, floor)
	}
}

var (
	sonnetT2 = router.Pin{Provider: "anthropic", ModelID: "claude-sonnet", Floor: domain.T2}
	opusT4   = router.Pin{Provider: "anthropic", ModelID: "claude-opus", Floor: domain.T4}
)

func TestRoutingSessionSeedAndWriteBack(t *testing.T) {
	h := newSessionHarness(t)
	now := time.Now().UTC()
	first, turn := h.start("client", "key", now)
	assertSeed(t, turn, nil, domain.T0)
	if turn.Conversation.SessionKey != "key" || turn.SessionUsage != nil {
		t.Fatalf("new session turn key=%q usage=%+v", turn.Conversation.SessionKey, turn.SessionUsage)
	}
	h.serve("client", first, sonnetT2, inference.Usage{Known: true, InputTokens: 100, CachedInputTokens: 900, CacheWriteInputTokens: 50, OutputTokens: 20})

	history := []inference.Item{
		{Type: "message", Role: "user", Text: "hi"},
		{Type: "message", Role: "assistant", Text: "hello"},
		{Type: "message", Role: "user", Text: "more"},
	}
	_, turn = h.start("client", "key", now, history...)
	assertSeed(t, turn, &sonnetT2, domain.T2)
	want := storage.SessionUsage{Provider: "anthropic", UncachedInput: 100, CacheRead: 900, CacheWrite: 50, Output: 20}
	if turn.SessionUsage == nil || *turn.SessionUsage != want {
		t.Fatalf("SessionUsage = %+v; want %+v", turn.SessionUsage, want)
	}
	if len(turn.Transcript) != len(history) {
		t.Fatalf("seeded turn transcript has %d items; want only the current request's %d", len(turn.Transcript), len(history))
	}

	_, unbound := h.start("client", "", now)
	assertSeed(t, unbound, nil, domain.T0)
	if unbound.Conversation.SessionKey != "" || unbound.SessionUsage != nil {
		t.Fatalf("unbound turn key=%q usage=%+v", unbound.Conversation.SessionKey, unbound.SessionUsage)
	}
}

func TestRoutingSessionRatchet(t *testing.T) {
	h := newSessionHarness(t)
	now := time.Now().UTC()
	a, _ := h.start("client", "key", now)
	b, _ := h.start("client", "key", now)
	h.serve("client", a, opusT4, inference.Usage{Known: true, OutputTokens: 1})
	// b was seeded before a committed, so its own conversation may still pin
	// a lower tier; the session must keep the higher pin and floor.
	h.serve("client", b, sonnetT2, inference.Usage{Known: true, OutputTokens: 2})

	c, turn := h.start("client", "key", now)
	assertSeed(t, turn, &opusT4, domain.T4)
	if turn.SessionUsage == nil || turn.SessionUsage.Output != 2 {
		t.Fatalf("SessionUsage = %+v; want the latest commit's usage", turn.SessionUsage)
	}
	if err := h.db.RaiseConversationFloor(h.ctx, "client", c, domain.T5); err != nil {
		t.Fatalf("RaiseConversationFloor() error = %v", err)
	}
	e, turn := h.start("client", "key", now)
	assertSeed(t, turn, &opusT4, domain.T5)
	if err := h.begin("client", e, sonnetT2); err == nil {
		t.Fatal("a seeded conversation accepted a decision below the session floor")
	}
}

func TestRoutingSessionIdleExpiry(t *testing.T) {
	h := newSessionHarness(t)
	now := time.Now().UTC()
	first, _ := h.start("client", "key", now)
	h.serve("client", first, opusT4, inference.Usage{Known: true, OutputTokens: 1})

	later := time.Now().UTC().Add(h.ttl + time.Second)
	_, turn := h.start("client", "key", later)
	assertSeed(t, turn, nil, domain.T0)
	if turn.SessionUsage != nil {
		t.Fatalf("expired session usage = %+v; want none", turn.SessionUsage)
	}

	h.ttl = 0
	second, _ := h.start("client", "forever", now)
	h.serve("client", second, sonnetT2, inference.Usage{Known: true})
	_, turn = h.start("client", "forever", now.Add(100*time.Hour))
	assertSeed(t, turn, &sonnetT2, domain.T2)
}

func TestDeleteExpiredSessions(t *testing.T) {
	h := newSessionHarness(t)
	now := time.Now().UTC()
	stale, _ := h.start("client", "stale", now)
	h.start("client", "fresh", now.Add(90*time.Minute))

	if removed, err := h.db.DeleteExpiredSessions(h.ctx, 0, now.Add(100*time.Hour)); err != nil || removed != 0 {
		t.Fatalf("DeleteExpiredSessions(0) = %d, %v; want no work", removed, err)
	}
	removed, err := h.db.DeleteExpiredSessions(h.ctx, time.Hour, now.Add(2*time.Hour))
	if err != nil || removed != 1 {
		t.Fatalf("DeleteExpiredSessions() = %d, %v; want 1", removed, err)
	}
	if _, err := h.db.GetConversationTurn(h.ctx, "client", stale); err != nil {
		t.Fatalf("expired session removed its conversation: %v", err)
	}
	_, turn := h.start("client", "fresh", now.Add(2*time.Hour))
	if turn.Conversation.SessionKey != "fresh" {
		t.Fatal("live session was not retained")
	}
}

func TestRoutingSessionClientIsolation(t *testing.T) {
	h := newSessionHarness(t)
	now := time.Now().UTC()
	first, _ := h.start("client", "key", now)
	h.serve("client", first, opusT4, inference.Usage{Known: true})
	_, turn := h.start("other-client", "key", now)
	assertSeed(t, turn, nil, domain.T0)
}

func TestCandidateScoresRoundTripSessionEstimates(t *testing.T) {
	db := openTestDB(t, filepath.Join(t.TempDir(), "scores.db"))
	record := sampleRecord()
	record.Decision.Candidates[0].ExpectedTurnCost, record.Decision.Candidates[0].HorizonCost = .25, .75
	insertRecord(t, db, record)
	got, err := db.GetRequest(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if score := got.Decision.Candidates[0]; score.ExpectedTurnCost != .25 || score.HorizonCost != .75 {
		t.Fatalf("round-tripped score=%+v", score)
	}
	// Rows scored before migration 007 have no expected cost; they ranked by
	// the worst case, so it reads back as their expected cost.
	if _, err := db.SQL().Exec("UPDATE candidate_scores SET expected_turn_cost = NULL, horizon_cost = 0"); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetRequest(context.Background(), record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if score := got.Decision.Candidates[0]; score.ExpectedTurnCost != score.DirectCost || score.HorizonCost != 0 {
		t.Fatalf("legacy score=%+v; want expected cost equal to direct cost", score)
	}
}
