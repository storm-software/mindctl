package executor

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/storm-software/mindctl/internal/accounts"
	"github.com/storm-software/mindctl/internal/conversation"
	"github.com/storm-software/mindctl/internal/oauth"
	"github.com/storm-software/mindctl/internal/provider"
	"github.com/storm-software/mindctl/internal/router"
	"github.com/storm-software/mindctl/internal/upstreamauth"
)

const maxAccountRounds = 32

func namedModel(model string) bool {
	return model != "" && model != automaticModel
}

func (s *Service) applyAlias(in *Input) {
	if s == nil || !namedModel(in.Request.Model) || len(s.aliases) == 0 {
		return
	}
	modelID, providerID, ok := accounts.Resolve(s.aliases, in.Request.Model)
	if !ok {
		return
	}
	in.Request.Model = modelID
	if providerID != "" && in.RequiredProvider == "" {
		in.RequiredProvider = providerID
	}
}

func (s *Service) executeNamed(ctx context.Context, in Input, turn conversation.Turn, decision router.Decision) (Output, error) {
	var last error
	round := 0
	for round <= maxAccountRounds {
		tried := []string{}
		progressed := false
		for {
			selection, err := s.selector.Select(ctx, s.accountRequest(in, decision, round, tried))
			if err != nil {
				var cooling *accounts.UnavailableError
				if !progressed && errors.As(err, &cooling) && cooling.RetryAfter > 0 {
					if waitErr := waitCooldown(ctx, cooling.RetryAfter); waitErr != nil {
						return Output{}, waitErr
					}
					break
				}
				if !progressed {
					if last != nil {
						return Output{}, last
					}
					return Output{}, err
				}
				round++
				break
			}
			progressed = true
			attemptCtx, ctxErr := s.attemptContext(ctx, selection)
			if ctxErr != nil {
				s.selector.ReportFailure(ctx, in.AccountSessionKey, selection.Account.ID, accounts.FailureAuth)
				return Output{}, ctxErr
			}
			if adapter, adapterErr := s.protocolAdapter(decision.Provider, selection.Account); adapterErr != nil {
				return Output{}, adapterErr
			} else if adapter != nil {
				attemptCtx = withProtocolAdapter(attemptCtx, adapter)
			}
			output, err := s.executeDecision(attemptCtx, in, turn, decision)
			if err == nil {
				s.selector.ReportSuccess(ctx, in.AccountSessionKey, selection.Account.ID)
				return output, nil
			}
			class := failureClass(err)
			s.selector.ReportFailure(ctx, in.AccountSessionKey, selection.Account.ID, class)
			last = err
			if class == accounts.FailurePermanent {
				return Output{}, err
			}
			tried = append(tried, selection.Account.ID)
		}
	}
	if last == nil {
		last = accounts.ErrNone
	}
	return Output{}, last
}

func waitCooldown(ctx context.Context, delay time.Duration) error {
	if delay > 30*time.Second {
		delay = 30 * time.Second
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func (s *Service) accountRequest(in Input, decision router.Decision, round int, tried []string) accounts.SelectRequest {
	return accounts.SelectRequest{
		ModelID: decision.ModelID, Provider: decision.Provider, SessionKey: in.AccountSessionKey,
		ParentSessionKey: in.ParentSessionKey, Guardian: in.Guardian, Round: round,
		Tried: append([]string(nil), tried...),
	}
}

type protocolAdapterKey struct{}

func withProtocolAdapter(ctx context.Context, adapter provider.Provider) context.Context {
	if adapter == nil {
		return ctx
	}
	return context.WithValue(ctx, protocolAdapterKey{}, adapter)
}

func protocolAdapterFrom(ctx context.Context) (provider.Provider, bool) {
	adapter, ok := ctx.Value(protocolAdapterKey{}).(provider.Provider)
	return adapter, ok && adapter != nil
}

func nativeProtocol(providerID string) string {
	switch providerID {
	case "anthropic":
		return "anthropic"
	case "gemini", "antigravity", "vertex":
		return "gemini"
	default:
		return "openai"
	}
}

// protocolAdapter returns the account's adapter when it speaks a different
// protocol than the catalog provider. A nil adapter means the registered
// provider client is correct. Automatic routing does not call this.
func (s *Service) protocolAdapter(providerID string, account accounts.Account) (provider.Provider, error) {
	if account.ID == "" || account.Protocol == "" {
		return nil, nil
	}
	if accounts.ProtocolFamily(account.Protocol) == accounts.ProtocolFamily(nativeProtocol(providerID)) {
		return nil, nil
	}
	if s == nil || s.protocols == nil {
		return nil, fmt.Errorf("executor: protocol %s is not available", account.Protocol)
	}
	adapter, ok := s.protocols(account.Protocol)
	if !ok || adapter == nil {
		return nil, fmt.Errorf("executor: protocol %s is not available", account.Protocol)
	}
	return adapter, nil
}

func (s *Service) attemptContext(ctx context.Context, selection accounts.Selection) (context.Context, error) {
	if selection.Account.Kind == accounts.KindServiceAccount && selection.Secret.PrivateKey != "" {
		token, err := oauth.GoogleAccessToken(ctx, oauth.ServiceAccount{
			ClientEmail: selection.Secret.ClientEmail,
			PrivateKey:  selection.Secret.PrivateKey,
			TokenURI:    selection.Secret.TokenURI,
		})
		if err != nil {
			return ctx, err
		}
		selection.Secret.APIKey = token
	}
	attempt := upstreamauth.Attempt{
		BaseURL: selection.Account.BaseURL, ProxyURL: selection.Account.ProxyURL, Headers: selection.Secret.Headers,
	}
	if selection.Account.ClaudeProfile {
		attempt.UserAgent = "claude-cli"
	}
	switch selection.Account.Provider {
	case "anthropic":
		if selection.Secret.AccessToken != "" {
			credential := upstreamauth.ClaudeCredential{AccessToken: selection.Secret.AccessToken}
			attempt.Claude = &credential
			ctx = upstreamauth.WithClaude(ctx, credential)
		} else if selection.Secret.APIKey != "" {
			attempt.APIKey = selection.Secret.APIKey
		}
	case "openai":
		if selection.Account.Kind == accounts.KindOAuth || selection.Account.Kind == accounts.KindPassthrough {
			if selection.Secret.AccessToken != "" && selection.Secret.AccountID != "" {
				credential := upstreamauth.ChatGPTCredential{AccessToken: selection.Secret.AccessToken, AccountID: selection.Secret.AccountID}
				attempt.ChatGPT = &credential
				ctx = upstreamauth.WithChatGPT(ctx, credential)
			}
		}
		if attempt.ChatGPT == nil && selection.Secret.APIKey != "" {
			attempt.APIKey = selection.Secret.APIKey
		} else if attempt.ChatGPT == nil && selection.Secret.AccessToken != "" {
			attempt.APIKey = selection.Secret.AccessToken
		}
	default:
		if selection.Secret.APIKey != "" {
			attempt.APIKey = selection.Secret.APIKey
		} else if selection.Secret.AccessToken != "" {
			attempt.APIKey = selection.Secret.AccessToken
		}
	}
	ctx = upstreamauth.WithAttempt(ctx, attempt)
	return conversation.WithAccountID(ctx, selection.Account.ID), nil
}

func failureClass(err error) accounts.FailureClass {
	var providerErr *provider.Error
	if errors.As(err, &providerErr) {
		switch providerErr.Kind {
		case provider.ErrorAuthentication:
			return accounts.FailureAuth
		case provider.ErrorRateLimit:
			return accounts.FailureQuota
		case provider.ErrorInvalidRequest, provider.ErrorSafetyRefusal:
			return accounts.FailurePermanent
		default:
			if providerErr.Status != 0 {
				return accounts.ClassFromStatus(providerErr.Status)
			}
			return accounts.FailureTransient
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return accounts.FailurePermanent
	}
	return accounts.FailureTransient
}
