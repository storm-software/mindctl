// Package storage defines the durable routing and encrypted-content boundary.
package storage

import (
	"context"
	"errors"
	"time"

	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
)

var ErrNotFound = errors.New("storage: request not found")

// Error adds a safe operation name without formatting its potentially sensitive
// cause. errors.Is/As can still inspect the original error.
type Error struct {
	Op  string
	Err error
}

func (e *Error) Error() string { return "storage: " + e.Op + " failed" }
func (e *Error) Unwrap() error { return e.Err }

// ReplaySnapshot freezes the complete policy input (including model prices and
// task-specific priors) and configuration used to produce a decision. Credential
// maps contain availability booleans, never credential values.
type ReplaySnapshot struct {
	Input  router.DecisionInput
	Policy router.PolicyConfig
}

// RequestRecord separates captured content from content-free routing telemetry.
// Put raw prompts, answers, rejected outputs and other captured data only in the
// four content fields. IDs, decision explanations, judgment and replay metadata
// must contain no captured content or credentials. CreatedAt defaults to the
// insertion time when zero; persisted timestamps are UTC.
//
// After retention, content fields are nil while telemetry remains readable.
type RequestRecord struct {
	ID              string
	CreatedAt       time.Time
	Prompt          []byte
	Answer          []byte
	RejectedOutputs [][]byte
	RawContent      []byte
	Decision        router.Decision
	Judgment        *domain.ClassifierJudgment
	Replay          ReplaySnapshot
}

// Tx is valid only during its WithTx callback. Any failed write prevents commit,
// including failures inadvertently ignored by the callback.
type Tx interface {
	InsertRequest(RequestRecord) error
}

// ConversationRecord contains durable, non-content conversation state.
// Pin and Floor deliberately remain outside encrypted payloads so routing can
// resume safely without decrypting captured content.
type ConversationRecord struct {
	ID, ClientID string
	CreatedAt    time.Time
	Pin          *router.Pin
	Floor        domain.Tier
}

// ResponseRecord is a gateway-owned response identifier within a conversation.
type ResponseRecord struct {
	ID, ConversationID, Status string
	Sequence                   int
	CreatedAt                  time.Time
}

// TranscriptItem is canonical content plus the provider that is allowed to
// consume its opaque ProviderData. An empty Provider means portable input.
type TranscriptItem struct {
	ResponseID string
	Position   int
	Provider   string
	Item       inference.Item
}

// ConversationTurn is the complete portable transcript available at a response.
type ConversationTurn struct {
	Conversation ConversationRecord
	Response     ResponseRecord
	Transcript   []TranscriptItem
}

type NewTurn struct {
	Conversation ConversationRecord
	Response     ResponseRecord
	Input        []inference.Item
}

// ProviderAttempt is written before any provider I/O. Decision is telemetry;
// successful Result and failed Error bodies are encrypted by the repository.
type ProviderAttempt struct {
	ID, ClientID, ResponseID, Status string
	Sequence                         int
	Decision                         router.Decision
	CreatedAt, CompletedAt           time.Time
	Result                           *inference.Result
	Error                            []byte
}

// HistoryFilter selects response history. Attempt filters must all match the
// same provider attempt. Zero times and a zero limit leave those bounds open.
type HistoryFilter struct {
	Provider, ModelID, Status string
	Since, Until              time.Time
	Limit                     int
}

// HistoryAttempt contains one model selection and its retained outcome.
// ContentRetained is false when the attempt is still running or retention has
// removed its encrypted result or error body.
type HistoryAttempt struct {
	ID, Provider, ModelID, Status, ProviderRequestID string
	Tier                                             domain.Tier
	CreatedAt                                        time.Time
	CompletedAt                                      *time.Time
	Result                                           *inference.Result
	Error                                            []byte
	ContentRetained                                  bool
}

// HistoryRecord is one client request and every model attempt made for it.
type HistoryRecord struct {
	ResponseID, ConversationID, Status string
	CreatedAt                          time.Time
	Request                            []inference.Item
	RequestContentRetained             bool
	Attempts                           []HistoryAttempt
}

// ConversationRepository adds the transactional persistence needed by the
// gateway-owned conversation service.
type ConversationRepository interface {
	CreateTurn(context.Context, NewTurn) error
	GetConversationTurn(context.Context, string, string) (ConversationTurn, error)
	BeginProviderAttempt(context.Context, ProviderAttempt) error
	CommitConversationResult(context.Context, string, string, router.Pin, inference.Result) error
	FailProviderAttempt(context.Context, string, string, string, string, []byte) error
	RaiseConversationFloor(context.Context, string, string, domain.Tier) error
}

type Repository interface {
	WithTx(context.Context, func(Tx) error) error
	GetRequest(context.Context, string) (RequestRecord, error)
	ListHistory(context.Context, HistoryFilter) ([]HistoryRecord, error)
	// DeleteExpiredContent deletes blobs strictly older than now-retention and
	// returns the blob count. Zero retention is unlimited and performs no work.
	DeleteExpiredContent(context.Context, time.Duration, time.Time) (int64, error)
	Ready(context.Context) error
}
