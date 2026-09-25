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
}

type service struct {
	store storage.ConversationRepository
}

// New creates a service over the encrypted durable storage boundary.
func New(store storage.ConversationRepository) Service { return &service{store: store} }

type Turn struct {
	ConversationID, ResponseID, ClientID string
	Pin                                  router.Pin
	Floor                                domain.Tier
	Transcript                           []inference.Item
	origins                              []string
	replay                               []inference.Item
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
			if item.Type == "reasoning" {
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
	if request.PreviousResponseID != "" {
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
		input[index].ProviderData = nil
		if input[index].ContinuationProvider == "" {
			input[index].EncryptedContent = nil
		}
	}

	if err := s.store.CreateTurn(ctx, storage.NewTurn{
		Conversation: storage.ConversationRecord{ID: conversationID, ClientID: clientID, CreatedAt: now},
		Response:     storage.ResponseRecord{ID: responseID, ConversationID: conversationID, CreatedAt: now, Status: "pending"},
		Input:        input,
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
	turn := Turn{ConversationID: record.Conversation.ID, ResponseID: record.Response.ID, ClientID: clientID, Floor: record.Conversation.Floor}
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
