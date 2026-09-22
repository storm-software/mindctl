package executor

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
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
	if err == nil || deps.provider.calls != 1 || !deps.writer.hasTerminalError || deps.conversations.floor != domain.T4 {
		t.Fatalf("calls=%d terminal=%v floor=%s err=%v", deps.provider.calls, deps.writer.hasTerminalError, deps.conversations.floor, err)
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
		executor: New(&fakeClassifier{Judgment: domain.JevJudgment{MinimumTier: domain.T4, TierConfidence: 1}}, router.NewPolicy(router.PolicyConfig{}), provider.NewRegistry(map[string]provider.Provider{"openai": adapter}), conversations),
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
	streams []provider.Stream
	calls   int
}

func (p *scriptedProvider) Execute(context.Context, domain.Model, inference.Request) (inference.Result, error) {
	return inference.Result{}, errors.New("unexpected non-streaming execution")
}

func (p *scriptedProvider) Stream(context.Context, domain.Model, inference.Request) (provider.Stream, error) {
	p.calls++
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
	turn      conversation.Turn
	floor     domain.Tier
	failCalls int
	committed inference.Result
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

func (s *streamConversations) FailAttempt(context.Context, conversation.Attempt, string, error) error {
	s.failCalls++
	return nil
}

func (s *streamConversations) RaiseFloor(_ context.Context, _ conversation.Turn, floor domain.Tier) error {
	if floor > s.floor {
		s.floor = floor
	}
	return nil
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
