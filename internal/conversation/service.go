// Package conversation owns gateway response IDs and portable transcript state.
package conversation

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"errors"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/savings"
	"github.com/storm-software/mindctl/internal/storage"
)

// ErrNotFound intentionally covers unknown and foreign-client response IDs.
var ErrNotFound = errors.New("conversation: response not found")

type Service interface {
	Start(context.Context, string, inference.Request) (Turn, error)
	Resume(context.Context, string, string) (Turn, error)
	BeginAttempt(context.Context, Turn, router.Decision) (Attempt, error)
	CommitResult(context.Context, Turn, router.Pin, inference.Result) error
	FailAttempt(context.Context, Attempt, string, error) error
	RaiseFloor(context.Context, Turn, domain.Tier) error
	RecordSavings(context.Context, Attempt, savings.Record) error
}

type service struct {
	store   storage.ConversationRepository
	options Options
}

// Options configures routing sessions. SessionIdleTTL is how long an idle
// session keeps its pin and floor; zero never expires.
type Options struct {
	SessionIdleTTL time.Duration
}

type trustedNativeInputKey struct{}

// WithTrustedNativeInput marks native continuation blocks decoded by the
// Messages API as eligible for provider-scoped encrypted transcript storage.
func WithTrustedNativeInput(ctx context.Context) context.Context {
	return context.WithValue(ctx, trustedNativeInputKey{}, true)
}

// New creates a service over the encrypted durable storage boundary.
func New(store storage.ConversationRepository) Service { return NewWithOptions(store, Options{}) }

// NewWithOptions creates a service with routing-session settings.
func NewWithOptions(store storage.ConversationRepository, options Options) Service {
	return &service{store: store, options: options}
}

// Turn is one response in a conversation. SessionKey is set when the
// conversation is bound to a routing session, whose last observed usage is
// SessionUsage.
type Turn struct {
	ConversationID, ResponseID, ClientID string
	SessionKey                           string
	Pin                                  router.Pin
	Floor                                domain.Tier
	Transcript                           []inference.Item
	SessionUsage                         *storage.SessionUsage
	origins                              []string
	replay                               []inference.Item
}

// AffinityID identifies the turn's cache affinity scope: its routing session
// when bound, otherwise its conversation.
func (t Turn) AffinityID() string {
	if t.SessionKey != "" {
		return t.SessionKey
	}
	return t.ConversationID
}

// TranscriptFor returns portable canonical content. Opaque continuation data
// is replayed only to its originating provider; a different provider retains
// the item but receives no incompatible continuation metadata.
func (t Turn) TranscriptFor(provider string) []inference.Item {
	items := make([]inference.Item, 0, len(t.replay))
	for i, item := range t.replay {
		origin := item.ContinuationProvider
		if origin == "" && i < len(t.origins) {
			origin = t.origins[i]
		}
		if origin != "" && origin != provider {
			// Reasoning and textless native carriers have no portable content.
			if item.Type == "reasoning" || item.Type == "agent_message" ||
				(item.Type == "message" && item.Text == "" && len(item.ImageURL) == 0) {
				continue
			}
			item.ProviderData = nil
			item.EncryptedContent = nil
		}
		items = append(items, cloneItem(item))
	}
	return items
}

type Attempt struct{ ID, ResponseID, ClientID string }

func (s *service) Start(ctx context.Context, clientID string, request inference.Request) (Turn, error) {
	if clientID == "" {
		return Turn{}, errors.New("conversation: client ID is required")
	}

	conversationID := randomID("conv_")
	sessionKey := request.SessionKey
	if request.PreviousResponseID != "" {
		// Gateway-owned continuation keeps its conversation; a session key
		// never rebinds it.
		sessionKey = ""
		previous, err := s.Resume(ctx, clientID, request.PreviousResponseID)
		if err != nil {
			return Turn{}, err
		}
		conversationID = previous.ConversationID
	}

	responseID := randomID("resp_")
	now := time.Now().UTC()
	input := make([]inference.Item, len(request.Input))
	for index, item := range request.Input {
		input[index] = cloneItem(item)
		// Caller-supplied metadata has no trusted provider provenance and must
		// never become a continuation payload for any adapter.
		if ctx.Value(trustedNativeInputKey{}) != true || input[index].ContinuationProvider != "anthropic" {
			input[index].ProviderData = nil
		}
		if input[index].ContinuationProvider == "" {
			input[index].EncryptedContent = nil
		}
	}

	var requestContext *storage.RequestContext
	if request.Instructions != "" || len(request.Tools) > 0 {
		requestContext = &storage.RequestContext{Instructions: request.Instructions, Tools: request.Tools}
	}

	if err := s.store.CreateTurn(ctx, storage.NewTurn{
		Conversation: storage.ConversationRecord{ID: conversationID, ClientID: clientID, CreatedAt: now},
		Response: storage.ResponseRecord{
			ID: responseID, ConversationID: conversationID, CreatedAt: now, Status: "pending",
			ExplicitModel: request.Model != "" && request.Model != inference.AutomaticModel,
		},
		Input:      input,
		Context:    requestContext,
		SessionKey: sessionKey, SessionIdleTTL: s.options.SessionIdleTTL,
	}); err != nil {
		return Turn{}, err
	}

	return s.resume(ctx, clientID, responseID)
}

func (s *service) Resume(ctx context.Context, clientID, responseID string) (Turn, error) {
	if clientID == "" || responseID == "" {
		return Turn{}, ErrNotFound
	}
	return s.resume(ctx, clientID, responseID)
}

func (s *service) resume(ctx context.Context, clientID, responseID string) (Turn, error) {
	record, err := s.store.GetConversationTurn(ctx, clientID, responseID)
	if errors.Is(err, storage.ErrNotFound) {
		return Turn{}, ErrNotFound
	}
	if err != nil {
		return Turn{}, err
	}
	turn := Turn{
		ConversationID: record.Conversation.ID, ResponseID: record.Response.ID, ClientID: clientID,
		SessionKey: record.Conversation.SessionKey, Floor: record.Conversation.Floor, SessionUsage: record.SessionUsage,
	}
	if record.Conversation.Pin != nil {
		turn.Pin = *record.Conversation.Pin
	}
	for _, entry := range record.Transcript {
		turn.replay = append(turn.replay, cloneItem(entry.Item))
		portable := cloneItem(entry.Item)
		portable.ProviderData = nil
		turn.Transcript = append(turn.Transcript, portable)
		turn.origins = append(turn.origins, entry.Provider)
	}
	return turn, nil
}

func (s *service) BeginAttempt(ctx context.Context, turn Turn, decision router.Decision) (Attempt, error) {
	attempt := Attempt{ID: randomID("att_"), ResponseID: turn.ResponseID, ClientID: turn.ClientID}
	err := s.store.BeginProviderAttempt(ctx, storage.ProviderAttempt{ID: attempt.ID, ClientID: turn.ClientID, ResponseID: turn.ResponseID, Decision: decision, CreatedAt: time.Now().UTC()})
	if errors.Is(err, storage.ErrNotFound) {
		return Attempt{}, ErrNotFound
	}
	if err != nil {
		return Attempt{}, err
	}
	return attempt, nil
}

func (s *service) CommitResult(ctx context.Context, turn Turn, pin router.Pin, result inference.Result) error {
	err := s.store.CommitConversationResult(ctx, turn.ClientID, turn.ResponseID, pin, result)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// RecordSavings stores savings telemetry for an attempt whose result has
// already committed.
func (s *service) RecordSavings(ctx context.Context, attempt Attempt, record savings.Record) error {
	err := s.store.RecordAttemptSavings(ctx, attempt.ClientID, attempt.ResponseID, attempt.ID, record)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func (s *service) FailAttempt(ctx context.Context, attempt Attempt, providerRequestID string, cause error) error {
	if cause == nil {
		cause = errors.New("provider attempt failed")
	}
	err := s.store.FailProviderAttempt(ctx, attempt.ClientID, attempt.ResponseID, attempt.ID, providerRequestID, []byte(cause.Error()))
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

// RaiseFloor records the minimum tier required after a stream failed after
// output reached the caller. It never changes the pinned provider/model.
func (s *service) RaiseFloor(ctx context.Context, turn Turn, floor domain.Tier) error {
	if !floor.Valid() {
		return errors.New("conversation: floor is invalid")
	}
	err := s.store.RaiseConversationFloor(ctx, turn.ClientID, turn.ResponseID, floor)
	if errors.Is(err, storage.ErrNotFound) {
		return ErrNotFound
	}
	return err
}

func randomID(prefix string) string {
	bytes := make([]byte, 18)
	if _, err := rand.Read(bytes); err != nil {
		panic("conversation: cryptographic randomness unavailable")
	}
	return prefix + base64.RawURLEncoding.EncodeToString(bytes)
}

func cloneItem(item inference.Item) inference.Item {
	item.ImageURL = append(item.ImageURL[:0:0], item.ImageURL...)
	item.Arguments = append(item.Arguments[:0:0], item.Arguments...)
	item.Output = append(item.Output[:0:0], item.Output...)
	item.Summary = append(item.Summary[:0:0], item.Summary...)
	item.EncryptedContent = append(item.EncryptedContent[:0:0], item.EncryptedContent...)
	item.Content = append([]inference.ContentPart(nil), item.Content...)
	for index := range item.Content {
		item.Content[index].ImageURL = append(item.Content[index].ImageURL[:0:0], item.Content[index].ImageURL...)
	}
	item.ProviderData = append(item.ProviderData[:0:0], item.ProviderData...)
	if bytes.Equal(bytes.TrimSpace(item.ProviderData), []byte("null")) {
		item.ProviderData = nil
	}
	return item
}
