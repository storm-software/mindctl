package executor

import (
	"bytes"
	"context"
	"errors"
	"io"
	"path/filepath"
	"testing"

	"github.com/storm-software/mindctl/internal/contentcrypto"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/storage/sqlite"
)

func TestStreamRetriesBeforeFirstVisibleEvent(t *testing.T) {
	deps := newStreamDependencies(failingStream(), successfulStream("ok"))
	err := deps.executor.Stream(context.Background(), deps.input(), deps.writer)
	if err != nil || deps.writer.text() != "ok" || deps.provider.calls != 2 || deps.conversations.failCalls != 1 {
		t.Fatalf("text=%q calls=%d failed=%d err=%v", deps.writer.text(), deps.provider.calls, deps.conversations.failCalls, err)
	}
}

func TestStreamReopensTheOnlyCandidateBeforeFirstVisibleEvent(t *testing.T) {
	deps := newStreamDependencies(failingStream(), successfulStream("ok"))
	deps.models = deps.models[:1]
	err := deps.executor.Stream(context.Background(), deps.input(), deps.writer)
	if err != nil || deps.writer.text() != "ok" || deps.provider.calls != 2 {
		t.Fatalf("text=%q calls=%d err=%v", deps.writer.text(), deps.provider.calls, err)
	}
}

func TestStreamDoesNotRetryAfterVisibleEvent(t *testing.T) {
	deps := newStreamDependencies(streamThenFail("partial"), successfulStream("replacement"))
	err := deps.executor.Stream(context.Background(), deps.input(), deps.writer)
	if err == nil || deps.provider.calls != 1 || !deps.writer.hasTerminalError || deps.conversations.floor != domain.T5 {
		t.Fatalf("calls=%d terminal=%v floor=%s err=%v", deps.provider.calls, deps.writer.hasTerminalError, deps.conversations.floor, err)
	}
}

func TestStreamRejectsPrematureEOFAfterVisibleOutput(t *testing.T) {
	stream := &scriptedStream{events: []inference.Event{{Type: "response.output_text.delta", Delta: "partial", ProviderRequestID: "upstream-request"}}, err: io.EOF}
	deps := newStreamDependencies(stream)
	deps.models = deps.models[:1]
	err := deps.executor.Stream(context.Background(), deps.input(), deps.writer)
	if err == nil || !deps.writer.hasTerminalError || deps.conversations.committed.Status != "" {
		t.Fatalf("terminal=%v committed=%+v err=%v", deps.writer.hasTerminalError, deps.conversations.committed, err)
	}
}

func TestStreamEscalatesExplicitModelAfterPreEmissionFailureWhenAllowed(t *testing.T) {
	deps := newStreamDependencies(failingStream(), successfulStream("ok"))
	input := deps.input()
	input.Request.Model = "first"
	allow := true
	input.AllowEscalation = &allow
	if err := deps.executor.Stream(context.Background(), input, deps.writer); err != nil {
		t.Fatal(err)
	}
	if len(deps.provider.models) < 2 || deps.provider.models[0] != "first" || deps.provider.models[1] != "second" {
		t.Fatalf("models=%v", deps.provider.models)
	}
}

func TestStreamStopsAfterEveryExplicitEscalationCandidateFails(t *testing.T) {
	deps := newStreamDependencies(failingStream(), failingStream())
	input := deps.input()
	input.Request.Model = "first"
	allow := true
	input.AllowEscalation = &allow

	err := deps.executor.Stream(context.Background(), input, deps.writer)
	if err == nil {
		t.Fatal("stream unexpectedly succeeded")
	}
	if deps.provider.calls != 2 || len(deps.provider.models) != 2 || deps.provider.models[0] != "first" || deps.provider.models[1] != "second" {
		t.Fatalf("calls=%d models=%v err=%v", deps.provider.calls, deps.provider.models, err)
	}
}

func TestStreamRaisesNextTierAfterVisibleFailureWithSQLiteConversation(t *testing.T) {
	conversations := sqliteStreamConversation(t)
	adapter := &scriptedProvider{streams: []provider.Stream{streamThenFail("partial")}}
	service := New(
		&fakeClassifier{Judgment: domain.ClassifierJudgment{MinimumTier: domain.T4, TierConfidence: 1}},
		router.NewPolicy(router.PolicyConfig{}),
		provider.NewRegistry(map[string]provider.Provider{"openai": adapter}),
		conversations,
	)
	model := streamModel("first", 0)
	// SQLite round-trips a nil RawMessage as JSON null, which the existing
	// feature extractor conservatively treats as an image field. This test is
	// concerned with persisted floor state, so keep that unrelated constraint
	// satisfiable.
	model.Capabilities.Images = true
	in := Input{
		ClientID: "client_stream",
		Models:   []domain.Model{model},
		Request:  inference.Request{Model: automaticModel, Stream: true, Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}},
	}
	err := service.Stream(context.Background(), in, &streamWriter{})
	if err == nil {
		t.Fatal("stream unexpectedly succeeded")
	}
	if len(adapter.requests) != 1 {
		t.Fatalf("requests=%d streamErr=%v", len(adapter.requests), err)
	}
	resumed, err := conversations.Resume(context.Background(), "client_stream", adapter.requests[0].ID)
	if err != nil || resumed.Floor != domain.T5 {
		t.Fatalf("floor=%s err=%v", resumed.Floor, err)
	}
}

func TestStreamFailureRetainsObservedProviderRequestID(t *testing.T) {
	deps := newStreamDependencies(streamThenFail("partial"))
	err := deps.executor.Stream(context.Background(), deps.input(), deps.writer)
	if err == nil || deps.conversations.failedProviderRequestID != "upstream-request" {
		t.Fatalf("providerRequestID=%q err=%v", deps.conversations.failedProviderRequestID, err)
	}
}

func TestStreamCancellationClosesUpstream(t *testing.T) {
	stream := &scriptedStream{err: context.Canceled}
	deps := newStreamDependencies(stream)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := deps.executor.Stream(ctx, deps.input(), deps.writer)
	if !errors.Is(err, context.Canceled) || !stream.closed {
		t.Fatalf("err=%v closed=%v", err, stream.closed)
	}
}

func TestStreamNormalizesProviderSpecificTextEvents(t *testing.T) {
	stream := &scriptedStream{events: []inference.Event{
		{Type: "content_block_delta", Delta: "normalized", ProviderRequestID: "anthropic-request"},
		{Type: "message_delta", Status: "completed", Usage: inference.Usage{InputTokens: 2, OutputTokens: 1, Known: true}, ProviderRequestID: "anthropic-request"},
	}, err: io.EOF}
	deps := newStreamDependencies(stream)
	deps.models = deps.models[:1]
	if err := deps.executor.Stream(context.Background(), deps.input(), deps.writer); err != nil {
		t.Fatal(err)
	}
	if len(deps.writer.events) != 2 || deps.writer.events[0].Type != "response.output_text.delta" || deps.writer.events[1].Type != "response.completed" || len(deps.conversations.committed.Output) != 1 || deps.conversations.committed.Output[0].Text != "normalized" {
		t.Fatalf("events=%+v committed=%+v", deps.writer.events, deps.conversations.committed)
	}
}

func TestStreamPersistsDoneTextWhenNoDeltaPrecedesIt(t *testing.T) {
	stream := &scriptedStream{events: []inference.Event{
		{Type: "response.output_text.done", ItemID: "item_1", Delta: "final", ProviderRequestID: "openai-request"},
		{Type: "response.completed", Status: "completed", ProviderRequestID: "openai-request"},
	}, err: io.EOF}
	deps := newStreamDependencies(stream)
	deps.models = deps.models[:1]
	if err := deps.executor.Stream(context.Background(), deps.input(), deps.writer); err != nil {
		t.Fatal(err)
	}
	if len(deps.conversations.committed.Output) != 1 || deps.conversations.committed.Output[0].Text != "final" {
		t.Fatalf("committed=%+v", deps.conversations.committed)
	}
}

func TestStreamMergesFunctionArgumentDeltasWithCompletedItem(t *testing.T) {
	stream := &scriptedStream{events: []inference.Event{
		{Type: "response.output_item.added", ItemID: "item_1", ItemType: "function_call"},
		{Type: "response.function_call_arguments.delta", ItemID: "item_1", ArgumentsDelta: `{"q":`},
		{Type: "response.function_call_arguments.delta", ItemID: "item_1", ArgumentsDelta: `"x"}`},
		{Type: "response.output_item.done", ItemID: "item_1", ItemType: "function_call", CallID: "call_1", Name: "lookup", ArgumentsDelta: `{"q":"x"}`},
		{Type: "response.completed", Status: "completed", ProviderRequestID: "openai-request"},
	}, err: io.EOF}
	deps := newStreamDependencies(stream)
	deps.models = deps.models[:1]

	if err := deps.executor.Stream(context.Background(), deps.input(), deps.writer); err != nil {
		t.Fatal(err)
	}
	output := deps.conversations.committed.Output
	if len(output) != 1 || output[0].Type != "function_call" || output[0].CallID != "call_1" || output[0].Name != "lookup" || string(output[0].Arguments) != `{"q":"x"}` {
		t.Fatalf("committed output=%+v", output)
	}
	if err := inference.ValidateRequest(inference.Request{Model: "mindctl-auto", Input: output}); err != nil {
		t.Fatalf("persisted output cannot be replayed: %v", err)
	}
}

type streamDependencies struct {
	executor      *Service
	provider      *scriptedProvider
	conversations *streamConversations
	writer        *streamWriter
	models        []domain.Model
}

func newStreamDependencies(streams ...provider.Stream) *streamDependencies {
	adapter := &scriptedProvider{streams: streams}
	conversations := &streamConversations{turn: conversation.Turn{ConversationID: "conv_stream", ResponseID: "resp_stream", ClientID: "client_stream", Floor: domain.T0}}
	models := []domain.Model{
		streamModel("first", 0),
		streamModel("second", 1),
	}
	return &streamDependencies{
		executor: New(&fakeClassifier{Judgment: domain.ClassifierJudgment{MinimumTier: domain.T4, TierConfidence: 1}}, router.NewPolicy(router.PolicyConfig{}), provider.NewRegistry(map[string]provider.Provider{"openai": adapter}), conversations),
		provider: adapter, conversations: conversations, writer: &streamWriter{}, models: models,
	}
}

func (d *streamDependencies) input() Input {
	return Input{ClientID: "client_stream", Models: append([]domain.Model(nil), d.models...), Request: inference.Request{Model: automaticModel, Stream: true, Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}}}
}

func streamModel(id string, order int) domain.Model {
	return domain.Model{ID: id, UpstreamID: id, Provider: "openai", Tier: domain.T4, Order: order, Available: true, ContextWindow: 32_000, MaxOutputTokens: 4_096, DefaultSuccessPrior: 1, Capabilities: domain.Capabilities{Text: true}}
}

type scriptedProvider struct {
	streams  []provider.Stream
	calls    int
	models   []string
	requests []inference.Request
}

func (p *scriptedProvider) Execute(context.Context, domain.Model, inference.Request) (inference.Result, error) {
	return inference.Result{}, errors.New("unexpected non-streaming execution")
}

func (p *scriptedProvider) Stream(_ context.Context, model domain.Model, request inference.Request) (provider.Stream, error) {
	p.calls++
	p.models = append(p.models, model.ID)
	p.requests = append(p.requests, request)
	if len(p.streams) == 0 {
		return nil, errors.New("unexpected stream call")
	}
	stream := p.streams[0]
	p.streams = p.streams[1:]
	return stream, nil
}

type scriptedStream struct {
	events []inference.Event
	err    error
	closed bool
}

func failingStream() *scriptedStream {
	return &scriptedStream{err: &provider.Error{Kind: provider.ErrorRetryable, Err: errors.New("temporary")}}
}

func successfulStream(text string) *scriptedStream {
	return &scriptedStream{events: []inference.Event{
		{Type: "response.output_text.delta", Delta: text, ProviderRequestID: "upstream-request"},
		{Type: "response.completed", Status: "completed", Usage: inference.Usage{InputTokens: 2, OutputTokens: 1, Known: true}, ProviderRequestID: "upstream-request"},
	}, err: io.EOF}
}

func streamThenFail(text string) *scriptedStream {
	return &scriptedStream{events: []inference.Event{{Type: "response.output_text.delta", Delta: text, ProviderRequestID: "upstream-request"}}, err: &provider.Error{Kind: provider.ErrorRetryable, Err: errors.New("temporary")}}
}

func (s *scriptedStream) Next(context.Context) (inference.Event, error) {
	if len(s.events) == 0 {
		return inference.Event{}, s.err
	}
	event := s.events[0]
	s.events = s.events[1:]
	return event, nil
}

func (s *scriptedStream) Close() error {
	s.closed = true
	return nil
}

type streamConversations struct {
	turn                    conversation.Turn
	floor                   domain.Tier
	failCalls               int
	failedProviderRequestID string
	committed               inference.Result
}

func (s *streamConversations) Start(_ context.Context, clientID string, _ inference.Request) (conversation.Turn, error) {
	turn := s.turn
	turn.ClientID = clientID
	return turn, nil
}

func (s *streamConversations) Resume(context.Context, string, string) (conversation.Turn, error) {
	return s.turn, nil
}

func (s *streamConversations) BeginAttempt(_ context.Context, turn conversation.Turn, decision router.Decision) (conversation.Attempt, error) {
	s.floor = decision.Tier
	return conversation.Attempt{ID: "att_stream", ResponseID: turn.ResponseID, ClientID: turn.ClientID}, nil
}

func (s *streamConversations) CommitResult(_ context.Context, _ conversation.Turn, _ router.Pin, result inference.Result) error {
	s.committed = result
	return nil
}

func (s *streamConversations) FailAttempt(_ context.Context, _ conversation.Attempt, providerRequestID string, _ error) error {
	s.failCalls++
	s.failedProviderRequestID = providerRequestID
	return nil
}

func (s *streamConversations) RaiseFloor(_ context.Context, _ conversation.Turn, floor domain.Tier) error {
	if floor > s.floor {
		s.floor = floor
	}
	return nil
}

func sqliteStreamConversation(t *testing.T) conversation.Service {
	t.Helper()
	keys, err := contentcrypto.New("test", map[string][]byte{"test": bytes.Repeat([]byte{1}, 32)})
	if err != nil {
		t.Fatal(err)
	}
	db, err := sqlite.Open(context.Background(), sqlite.Options{Path: filepath.Join(t.TempDir(), "stream.db"), Keyring: keys})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return conversation.New(db)
}

type streamWriter struct {
	events           []inference.Event
	hasTerminalError bool
}

func (w *streamWriter) Start(StreamMetadata) error { return nil }

func (w *streamWriter) WriteEvent(_ context.Context, event inference.Event) error {
	w.events = append(w.events, event)
	return nil
}

func (w *streamWriter) WriteTerminalError(context.Context, error) error {
	w.hasTerminalError = true
	return nil
}

func (w *streamWriter) text() string {
	var result string
	for _, event := range w.events {
		result += event.Delta
	}
	return result
}
