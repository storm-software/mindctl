package executor

import (
	"context"
	"errors"
	"testing"

	"github.com/storm-software/mindctl/internal/classifier"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
)

func TestExecuteSkipsJevForCompatibleConversationPin(t *testing.T) {
	deps := fakeDepsWithPin("gpt-pinned", domain.T3)
	got, err := deps.Executor.Execute(context.Background(), deps.input(inputWithPreviousResponse()))
	if err != nil || deps.Classifier.Calls != 0 || deps.OpenAI.Calls != 1 || got.Result.Model != "gpt-pinned" {
		t.Fatalf("got=%+v deps=%+v err=%v", got, deps, err)
	}
}

func TestExecuteUsesJevForUncertainAutomaticRequest(t *testing.T) {
	deps := fakeDepsWithJudgment(domain.T4)
	got, err := deps.Executor.Execute(context.Background(), deps.input(newAutomaticInput()))
	if err != nil || deps.Classifier.Calls != 1 || got.Decision.Tier < domain.T4 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestExecuteHonorsConcreteModelWithoutClassifier(t *testing.T) {
	deps := fakeDeps()
	got, err := deps.Executor.Execute(context.Background(), deps.input(concreteModelInput("claude-test")))
	if err != nil || got.Decision.ModelID != "claude-test" || deps.Classifier.Calls != 0 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestAutoRouteExcludesProviderWithoutHostedTool(t *testing.T) {
	deps := fakeDepsWithHostedToolModels()
	got, err := deps.Executor.Execute(context.Background(), deps.input(hostedToolInput("web_search")))
	if err != nil || got.Decision.Provider != "openai" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestAutoRouteExcludesUnavailableModel(t *testing.T) {
	deps := fakeDepsWithUnavailableCheapModel()
	got, err := deps.Executor.Execute(context.Background(), deps.input(newAutomaticInput()))
	if err != nil || got.Decision.ModelID == "unavailable-cheap" {
		t.Fatalf("got=%+v err=%v", got, err)
	}
}

func TestExecuteRejectsBeforeProviderWhenAttemptCannotBePersisted(t *testing.T) {
	deps := fakeDeps()
	deps.Conversations.BeginAttemptErr = errors.New("sqlite unavailable")
	_, err := deps.Executor.Execute(context.Background(), deps.input(newAutomaticInput()))
	if err == nil || deps.OpenAI.Calls != 0 {
		t.Fatalf("calls=%d err=%v", deps.OpenAI.Calls, err)
	}
}

func TestExecuteFailsAttemptWhenProviderFails(t *testing.T) {
	deps := fakeDeps()
	deps.OpenAI.Err = errors.New("provider unavailable")
	_, err := deps.Executor.Execute(context.Background(), deps.input(newAutomaticInput()))
	if err == nil || deps.Conversations.FailCalls != 1 {
		t.Fatalf("fail calls=%d err=%v", deps.Conversations.FailCalls, err)
	}
}

func TestExecuteRewritesAndCommitsOnlyGatewayResult(t *testing.T) {
	deps := fakeDeps()
	deps.OpenAI.Result = inference.Result{ID: "provider-response", Model: "provider-model", ProviderRequestID: "upstream-1", Status: "completed"}
	got, err := deps.Executor.Execute(context.Background(), deps.input(newAutomaticInput()))
	if err != nil || got.Result.ID != deps.Conversations.Turn.ResponseID || got.Result.Model != got.Decision.ModelID || got.Result.ProviderRequestID != "" || deps.Conversations.Committed.ID != got.Result.ID || deps.Conversations.Committed.Model != got.Result.Model || deps.Conversations.Committed.ProviderRequestID != "upstream-1" {
		t.Fatalf("got=%+v committed=%+v err=%v", got, deps.Conversations.Committed, err)
	}
}

func TestExecuteRejectsUnacceptableProviderStatusBeforeCommit(t *testing.T) {
	deps := fakeDeps()
	deps.OpenAI.Result.Status = "failed"
	_, err := deps.Executor.Execute(context.Background(), deps.input(newAutomaticInput()))
	if err == nil || deps.Conversations.CommitCalls != 0 || deps.Conversations.FailCalls != 1 {
		t.Fatalf("commit=%d failed=%d err=%v", deps.Conversations.CommitCalls, deps.Conversations.FailCalls, err)
	}
}

type fakeDependencies struct {
	Executor      *Service
	Classifier    *fakeClassifier
	OpenAI        *fakeProvider
	Anthropic     *fakeProvider
	Conversations *fakeConversations
	Models        []domain.Model
}

func (d *fakeDependencies) input(input Input) Input {
	input.Models = append([]domain.Model(nil), d.Models...)
	return input
}

func fakeDeps() *fakeDependencies {
	classifier := &fakeClassifier{Judgment: domain.JevJudgment{MinimumTier: domain.T4, TierConfidence: 1, TaskType: domain.TaskGeneration}}
	openai := &fakeProvider{Result: inference.Result{ID: "provider-response", Model: "provider-model", ProviderRequestID: "upstream-1", Status: "completed"}}
	anthropic := &fakeProvider{Result: inference.Result{ID: "provider-response", Model: "provider-model", ProviderRequestID: "upstream-2", Status: "completed"}}
	conversations := &fakeConversations{Turn: conversation.Turn{ConversationID: "conv_test", ResponseID: "resp_test", ClientID: "client_test", Floor: domain.T0}}
	deps := &fakeDependencies{Classifier: classifier, OpenAI: openai, Anthropic: anthropic, Conversations: conversations}
	deps.Models = []domain.Model{
		fakeModel("gpt-test", "openai", domain.T4, 1),
		fakeModel("claude-test", "anthropic", domain.T4, 2),
	}
	deps.Executor = New(classifier, router.NewPolicy(router.PolicyConfig{}), provider.NewRegistry(map[string]provider.Provider{"openai": openai, "anthropic": anthropic}), conversations)
	return deps
}

func fakeDepsWithPin(modelID string, tier domain.Tier) *fakeDependencies {
	deps := fakeDeps()
	deps.Models = []domain.Model{fakeModel(modelID, "openai", tier, 1), fakeModel("stronger", "anthropic", domain.T4, 2)}
	deps.Conversations.Turn.Pin = router.Pin{ModelID: modelID, Provider: "openai", Floor: tier}
	deps.Conversations.Turn.Floor = tier
	return deps
}

func fakeDepsWithJudgment(tier domain.Tier) *fakeDependencies {
	deps := fakeDeps()
	deps.Classifier.Judgment.MinimumTier = tier
	return deps
}

func fakeDepsWithHostedToolModels() *fakeDependencies {
	deps := fakeDeps()
	deps.Models = []domain.Model{
		fakeModel("openai-tools", "openai", domain.T4, 2),
		fakeModel("anthropic-no-tools", "anthropic", domain.T4, 1),
	}
	deps.Models[0].Capabilities.HostedTools = map[string]bool{"web_search": true}
	return deps
}

func fakeDepsWithUnavailableCheapModel() *fakeDependencies {
	deps := fakeDeps()
	deps.Models = []domain.Model{
		fakeModel("unavailable-cheap", "openai", domain.T4, 1),
		fakeModel("available", "anthropic", domain.T4, 2),
	}
	deps.Models[0].Available = false
	return deps
}

func fakeModel(id, providerID string, tier domain.Tier, order int) domain.Model {
	return domain.Model{ID: id, UpstreamID: id, Provider: providerID, Tier: tier, Order: order, Available: true, ContextWindow: 32_000, MaxOutputTokens: 4_096, DefaultSuccessPrior: 1, Capabilities: domain.Capabilities{Text: true}, Pricing: domain.Pricing{}}
}

func newAutomaticInput() Input {
	return Input{ClientID: "client_test", Request: inference.Request{Model: "mindctl-auto", Input: []inference.Item{{Type: "message", Role: "user", Text: "hello"}}}}
}

func inputWithPreviousResponse() Input {
	in := newAutomaticInput()
	in.Request.PreviousResponseID = "resp_previous"
	return in
}

func concreteModelInput(model string) Input {
	in := newAutomaticInput()
	in.Request.Model = model
	return in
}

func hostedToolInput(tool string) Input {
	in := newAutomaticInput()
	in.Features.HostedToolTypes = []string{tool}
	return in
}

type fakeClassifier struct {
	Calls    int
	Judgment domain.JevJudgment
	Err      error
}

func (f *fakeClassifier) Classify(context.Context, classifier.Input) (domain.JevJudgment, error) {
	f.Calls++
	return f.Judgment, f.Err
}

type fakeProvider struct {
	Calls   int
	Result  inference.Result
	Err     error
	Request inference.Request
}

func (f *fakeProvider) Execute(_ context.Context, _ domain.Model, request inference.Request) (inference.Result, error) {
	f.Calls++
	f.Request = request
	return f.Result, f.Err
}

func (f *fakeProvider) Stream(context.Context, domain.Model, inference.Request) (provider.Stream, error) {
	return nil, errors.New("streaming is not part of this executor test")
}

type fakeConversations struct {
	Turn            conversation.Turn
	BeginAttemptErr error
	CommitErr       error
	BeginCalls      int
	CommitCalls     int
	FailCalls       int
	Committed       inference.Result
}

func (f *fakeConversations) Start(_ context.Context, clientID string, request inference.Request) (conversation.Turn, error) {
	turn := f.Turn
	turn.ClientID = clientID
	return turn, nil
}

func (f *fakeConversations) Resume(context.Context, string, string) (conversation.Turn, error) {
	return f.Turn, nil
}

func (f *fakeConversations) BeginAttempt(_ context.Context, turn conversation.Turn, _ router.Decision) (conversation.Attempt, error) {
	f.BeginCalls++
	if f.BeginAttemptErr != nil {
		return conversation.Attempt{}, f.BeginAttemptErr
	}
	return conversation.Attempt{ID: "att_test", ResponseID: turn.ResponseID, ClientID: turn.ClientID}, nil
}

func (f *fakeConversations) CommitResult(_ context.Context, _ conversation.Turn, _ router.Pin, result inference.Result) error {
	f.CommitCalls++
	f.Committed = result
	return f.CommitErr
}

func (f *fakeConversations) FailAttempt(context.Context, conversation.Attempt, string, error) error {
	f.FailCalls++
	return nil
}

func (f *fakeConversations) RaiseFloor(context.Context, conversation.Turn, domain.Tier) error {
	return nil
}
