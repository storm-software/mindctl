package executor

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/headroom"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage/sqlite"
)

// sessionProvider serves every request successfully, streaming or not, and
// appends the serving model to a log shared by all providers. While
// *failStreams is positive, streams fail retryably before any output.
type sessionProvider struct {
	served      *[]string
	failStreams *int
	usage       *inference.Usage
}

func (p sessionProvider) Execute(_ context.Context, model domain.Model, _ inference.Request) (inference.Result, error) {
	*p.served = append(*p.served, model.ID)
	return inference.Result{Status: "completed", ProviderRequestID: "upstream", Usage: *p.usage,
		Output: []inference.Item{{Type: "message", Role: "assistant", Text: "ok"}}}, nil
}

func (p sessionProvider) Stream(_ context.Context, model domain.Model, _ inference.Request) (provider.Stream, error) {
	*p.served = append(*p.served, model.ID)
	if *p.failStreams > 0 {
		*p.failStreams--
		return failingStream(), nil
	}
	return successfulStream("ok"), nil
}

type recordingCompressor struct{ affinity []string }

func (c *recordingCompressor) Compress(_ context.Context, _ domain.Model, affinityID, _ string, request inference.Request) (inference.Request, headroom.Metrics, error) {
	c.affinity = append(c.affinity, affinityID)
	return request, headroom.Metrics{}, nil
}

type sessionHarness struct {
	t           *testing.T
	executor    *Service
	classifier  *fakeClassifier
	compressor  *recordingCompressor
	models      []domain.Model
	served      []string
	failStreams int
	usage       inference.Usage
	configure   func(*Input)
	decision    router.Decision
	stream      bool
}

func newSessionHarness(t *testing.T, stream bool) *sessionHarness {
	t.Helper()
	keys, err := contentcrypto.New("test", map[string][]byte{"test": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: filepath.Join(t.TempDir(), "session.db"), Keyring: keys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	h := &sessionHarness{
		t: t, stream: stream, compressor: &recordingCompressor{}, usage: inference.Usage{Known: true, InputTokens: 2, OutputTokens: 1},
		classifier: &fakeClassifier{Judgment: domain.ClassifierJudgment{MinimumTier: domain.T1, TierConfidence: 1}},
	}
	small, large := fakeModel("small", "openai", domain.T1, 1), fakeModel("large", "anthropic", domain.T4, 2)
	small.Capabilities.Functions, large.Capabilities.Functions = true, true
	h.models = []domain.Model{small, large}
	adapter := sessionProvider{served: &h.served, failStreams: &h.failStreams, usage: &h.usage}
	h.executor = NewWithCompressor(h.classifier, router.NewPolicy(router.PolicyConfig{}),
		provider.NewRegistry(map[string]provider.Provider{"openai": adapter, "anthropic": adapter}),
		conversation.NewWithOptions(db, conversation.Options{SessionIdleTTL: time.Hour}), h.compressor, nil)
	return h
}

// turn serves one request and returns the model that served it.
func (h *sessionHarness) turn(judgment domain.Tier, sessionKey string, items ...inference.Item) string {
	h.t.Helper()
	h.classifier.Judgment.MinimumTier = judgment
	in := Input{ClientID: "client", Models: append([]domain.Model(nil), h.models...), Request: inference.Request{
		Model: automaticModel, Stream: h.stream, Input: items, SessionKey: sessionKey,
	}}
	if h.configure != nil {
		h.configure(&in)
	}
	var err error
	if h.stream {
		err = h.executor.Stream(context.Background(), in, &streamWriter{})
	} else {
		var out Output
		out, err = h.executor.Execute(context.Background(), in)
		h.decision = out.Decision
	}
	if err != nil {
		h.t.Fatalf("turn error = %v", err)
	}
	return h.served[len(h.served)-1]
}

func userText(text string) inference.Item {
	return inference.Item{Type: "message", Role: "user", Text: text}
}

func assistantText(text string) inference.Item {
	return inference.Item{Type: "message", Role: "assistant", Text: text}
}

func toolRound(id string) []inference.Item {
	return []inference.Item{
		{Type: "function_call", CallID: id, Name: "read", Arguments: []byte(`{}`)},
		{Type: "function_call_output", CallID: id, Output: []byte(`"contents"`)},
		// Claude Code appends reminder text to tool-result messages.
		userText("<system-reminder>files changed</system-reminder>"),
	}
}

func history(parts ...[]inference.Item) []inference.Item {
	var items []inference.Item
	for _, part := range parts {
		items = append(items, part...)
	}
	return items
}

func TestNewUserTurn(t *testing.T) {
	for _, tc := range []struct {
		name  string
		items []inference.Item
		want  bool
	}{
		{"first message", []inference.Item{userText("hi")}, true},
		{"after assistant reply", history([]inference.Item{userText("hi"), assistantText("ok"), userText("more")}), true},
		{"tool continuation with reminder", history([]inference.Item{userText("hi")}, toolRound("a")), false},
		{"tool continuation", history([]inference.Item{userText("hi")}, toolRound("a")[:2]), false},
		{"user after tool round and reply", history([]inference.Item{userText("hi")}, toolRound("a"), []inference.Item{assistantText("done"), userText("next")}), true},
		{"no user text", []inference.Item{assistantText("ok")}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := newUserTurn(tc.items); got != tc.want {
				t.Fatalf("newUserTurn() = %v; want %v", got, tc.want)
			}
		})
	}
}

func testSessionContinuationSkipsClassifier(t *testing.T, stream bool) {
	h := newSessionHarness(t, stream)
	first := []inference.Item{userText("fix the parser")}
	if got := h.turn(domain.T1, "session", first...); got != "small" || h.classifier.Calls != 1 {
		t.Fatalf("turn 1 served %s with %d classifier calls", got, h.classifier.Calls)
	}
	// A continuation is routed by the session pin even when the classifier
	// would now demand a higher tier.
	if got := h.turn(domain.T4, "session", history(first, toolRound("a"))...); got != "small" || h.classifier.Calls != 1 {
		t.Fatalf("tool continuation served %s with %d classifier calls", got, h.classifier.Calls)
	}
	third := history(first, toolRound("a"), []inference.Item{assistantText("done"), userText("now add tests")})
	if got := h.turn(domain.T1, "session", third...); got != "small" || h.classifier.Calls != 2 {
		t.Fatalf("new user turn served %s with %d classifier calls", got, h.classifier.Calls)
	}
	if h.classifier.LastInput.Prompt != "now add tests" {
		t.Fatalf("session classifier prompt = %q; want only the current user turn", h.classifier.LastInput.Prompt)
	}
	for _, id := range h.compressor.affinity {
		if id != "session" {
			t.Fatalf("compressor affinity IDs = %v; want the session key", h.compressor.affinity)
		}
	}
}

func testSessionUserTurnCanOnlyRaise(t *testing.T, stream bool) {
	h := newSessionHarness(t, stream)
	first := []inference.Item{userText("fix the parser")}
	h.turn(domain.T1, "session", first...)
	second := history(first, []inference.Item{assistantText("done"), userText("redesign everything")})
	if got := h.turn(domain.T4, "session", second...); got != "large" {
		t.Fatalf("higher judgment served %s; want an upward switch", got)
	}
	third := history(second, []inference.Item{assistantText("done"), userText("thanks")})
	if got := h.turn(domain.T0, "session", third...); got != "large" {
		t.Fatalf("lower judgment served %s; the session must not downgrade", got)
	}
	if got := h.turn(domain.T0, "session", history(third, toolRound("b"))...); got != "large" {
		t.Fatalf("continuation after upgrade served %s", got)
	}
}

func TestSessionContinuationSkipsClassifier(t *testing.T) {
	testSessionContinuationSkipsClassifier(t, false)
}
func TestStreamSessionContinuationSkipsClassifier(t *testing.T) {
	testSessionContinuationSkipsClassifier(t, true)
}
func TestSessionUserTurnCanOnlyRaise(t *testing.T)       { testSessionUserTurnCanOnlyRaise(t, false) }
func TestStreamSessionUserTurnCanOnlyRaise(t *testing.T) { testSessionUserTurnCanOnlyRaise(t, true) }

func TestSessionsAreIndependentAndUnboundRequestsUnchanged(t *testing.T) {
	h := newSessionHarness(t, false)
	h.turn(domain.T4, "session-a", userText("hard task"))
	if got := h.turn(domain.T1, "session-b", userText("easy task")); got != "small" {
		t.Fatalf("second session served %s; want its own routing", got)
	}
	calls := h.classifier.Calls
	// Without a session every request is classified, as before sessions.
	h.turn(domain.T1, "", history([]inference.Item{userText("hi")}, toolRound("a"))...)
	if h.classifier.Calls != calls+1 {
		t.Fatal("unbound request skipped the classifier")
	}
	if last := h.compressor.affinity[len(h.compressor.affinity)-1]; !strings.HasPrefix(last, "conv_") {
		t.Fatalf("unbound compressor affinity = %q; want the conversation ID", last)
	}
}

func TestStreamSessionRetryWritesBackServedModel(t *testing.T) {
	h := newSessionHarness(t, true)
	// Both attempts on the first choice fail before output, so the stream
	// escalates to the stronger model, which the session must then keep.
	h.failStreams = 2
	first := []inference.Item{userText("fix the parser")}
	if got := h.turn(domain.T1, "session", first...); got != "large" {
		t.Fatalf("retried stream served %s; want the escalated candidate", got)
	}
	if got := h.turn(domain.T1, "session", history(first, toolRound("a"))...); got != "large" {
		t.Fatalf("session after retry served %s; want the model that served", got)
	}
}

func TestSessionEstimatesFromObservedUsage(t *testing.T) {
	h := newSessionHarness(t, false)
	// Output is the only nonzero price, so a candidate's expected turn cost
	// in USD equals the expected output tokens.
	for index := range h.models {
		h.models[index].Pricing = domain.Pricing{OutputPerMillion: 1e6}
	}
	h.configure = func(in *Input) {
		in.Request.MaxOutputTokens = 4_000
		in.ExpectedOutputTokens = 2_000
		in.Session = SessionSettings{HorizonTurns: 3, DefaultCacheHitRatio: .8}
	}
	expectedTurnCost := func() float64 {
		for _, score := range h.decision.Candidates {
			if score.ModelID == h.decision.ModelID {
				return score.ExpectedTurnCost
			}
		}
		t.Fatalf("no score for %s in %+v", h.decision.ModelID, h.decision.Candidates)
		return 0
	}

	h.usage = inference.Usage{Known: true, InputTokens: 100_000, CachedInputTokens: 90_000, OutputTokens: 1_500}
	first := []inference.Item{userText("fix the parser")}
	h.turn(domain.T1, "session", first...)
	if !hasDecisionReason(h.decision, "session horizon: turns=3 cache_hit_ratio=0.80") ||
		!hasDecisionReason(h.decision, "session cache hit ratio source: default") || expectedTurnCost() != 2_000 {
		t.Fatalf("first turn reasons=%v expected turn cost=%v", h.decision.Reasons, expectedTurnCost())
	}

	h.turn(domain.T1, "session", history(first, toolRound("a"))...)
	if !hasDecisionReason(h.decision, "session horizon: turns=3 cache_hit_ratio=0.90") ||
		!hasDecisionReason(h.decision, "session cache hit ratio source: observed") || expectedTurnCost() != 1_500 {
		t.Fatalf("second turn reasons=%v expected turn cost=%v", h.decision.Reasons, expectedTurnCost())
	}

	h.turn(domain.T1, "", userText("unbound"))
	for _, reason := range h.decision.Reasons {
		if strings.HasPrefix(reason, "session") {
			t.Fatalf("unbound request has session reason %q", reason)
		}
	}
	if expectedTurnCost() != 2_000 {
		t.Fatalf("unbound expected turn cost=%v; want the configured expected output", expectedTurnCost())
	}
}

func hasDecisionReason(decision router.Decision, want string) bool {
	for _, reason := range decision.Reasons {
		if reason == want {
			return true
		}
	}
	return false
}
