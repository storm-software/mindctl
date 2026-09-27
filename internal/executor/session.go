package executor

import (
	"strings"

	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/domain"
	"github.com/storm-software/mindctl/internal/inference"
	"github.com/storm-software/mindctl/internal/router"
)

// sessionFeatures returns the turn's routing features with its expected output
// and, for a session-bound turn, the session estimate. A session's last
// observed output and cache hit ratio replace the configured defaults.
func sessionFeatures(in Input, turn conversation.Turn) (domain.RequestFeatures, *router.SessionEstimate) {
	features := normalizedFeatures(in.Features, in.Request, turn)
	usage := turn.SessionUsage
	features.ExpectedOutputTokens = in.ExpectedOutputTokens
	if usage != nil && usage.Output > 0 {
		features.ExpectedOutputTokens = usage.Output
	}
	features = features.Normalize()
	if turn.SessionKey == "" {
		return features, nil
	}
	estimate := &router.SessionEstimate{HorizonTurns: in.Session.HorizonTurns, CacheHitRatio: in.Session.DefaultCacheHitRatio}
	if usage != nil {
		if total := usage.UncachedInput + usage.CacheRead + usage.CacheWrite; total > 0 {
			estimate.CacheHitRatio, estimate.Observed = float64(usage.CacheRead)/float64(total), true
		}
	}
	return features, estimate
}

// modelOutput reports whether item was produced by a model rather than by the
// user or a tool.
func modelOutput(item inference.Item) bool {
	if item.Role == "assistant" || item.Type == "reasoning" {
		return true
	}
	return strings.HasSuffix(item.Type, "_call") && !strings.HasSuffix(item.Type, "_call_output")
}

// currentUserTurn returns the items after the last model output: what the
// caller added since the model last spoke.
func currentUserTurn(items []inference.Item) []inference.Item {
	for index := len(items) - 1; index >= 0; index-- {
		if modelOutput(items[index]) {
			return items[index+1:]
		}
	}
	return items
}

// newUserTurn reports whether items end with a new user request rather than
// tool results continuing the model's work. Clients may append user text such
// as system reminders to tool results, so any tool output makes the turn a
// continuation.
func newUserTurn(items []inference.Item) bool {
	user := false
	for _, item := range currentUserTurn(items) {
		if strings.HasSuffix(item.Type, "_call_output") {
			return false
		}
		if item.Role == "user" && item.Type == "message" {
			user = true
		}
	}
	return user
}

// classifierSkipReason reports whether a compatible pin may route the turn
// without a classifier call. A session-bound turn is classified when the user
// makes a new request, so a session can escalate beyond its first request.
func classifierSkipReason(turn conversation.Turn, request inference.Request) (string, bool) {
	if turn.SessionKey == "" {
		return "pin_no_session", true
	}
	if !newUserTurn(request.Input) {
		return "pin_continuation", true
	}
	return "", false
}

// sessionKeyPrefix is a content-free, log-safe session correlation value.
func sessionKeyPrefix(turn conversation.Turn) string {
	if len(turn.SessionKey) < 8 {
		return ""
	}
	return turn.SessionKey[:8]
}
