package accounts

import (
	"slices"
	"strings"
	"sync"
	"time"
)

// Conductor picks an account for a named model. It is safe for concurrent use
// and performs no network I/O.
type Conductor struct {
	mu       sync.Mutex
	settings Settings
	accounts []Account
	cursor   int
	bindings map[string]binding
	cooldown map[string]time.Time
	now      func() time.Time
}

type binding struct {
	AccountID, ModelID string
	Expires            time.Time
}

// NewConductor copies the account list. now may be nil and then defaults to time.Now.
func NewConductor(settings Settings, accounts []Account, now func() time.Time) *Conductor {
	if now == nil {
		now = time.Now
	}
	copied := make([]Account, len(accounts))
	copy(copied, accounts)
	slices.SortFunc(copied, func(a, b Account) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		return strings.Compare(a.ID, b.ID)
	})
	return &Conductor{
		settings: settings.Normalize(),
		accounts: copied,
		bindings: map[string]binding{},
		cooldown: map[string]time.Time{},
		now:      now,
	}
}

// ReplaceAccounts swaps the pool without clearing session bindings or cooldown.
func (c *Conductor) ReplaceAccounts(accounts []Account) {
	if c == nil {
		return
	}
	copied := make([]Account, len(accounts))
	copy(copied, accounts)
	slices.SortFunc(copied, func(a, b Account) int {
		if a.Priority != b.Priority {
			return a.Priority - b.Priority
		}
		return strings.Compare(a.ID, b.ID)
	})
	c.mu.Lock()
	c.accounts = copied
	c.mu.Unlock()
}

// Accounts returns a copy of the current pool.
func (c *Conductor) Accounts() []Account {
	if c == nil {
		return []Account{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]Account, len(c.accounts))
	copy(out, c.accounts)
	return out
}

// HasEligible reports whether Select would find an account, without consuming
// the round-robin cursor or a session binding.
func (c *Conductor) HasEligible(req SelectRequest) bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	_, _, ok := c.candidate(req, false)
	return ok
}

// Select returns the next account for a named model.
func (c *Conductor) Select(req SelectRequest) (Account, error) {
	if c == nil {
		return Account{}, ErrNone
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	account, retryAfter, ok := c.candidate(req, true)
	if ok {
		c.bind(req, account)
		return account, nil
	}
	if retryAfter > 0 {
		return Account{}, &UnavailableError{RetryAfter: retryAfter}
	}
	return Account{}, ErrNone
}

// ReportSuccess extends a session binding after a successful attempt.
func (c *Conductor) ReportSuccess(sessionKey, accountID string) {
	if c == nil || sessionKey == "" || !c.settings.SessionAffinity {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	current, ok := c.bindings[sessionKey]
	if !ok || current.AccountID != accountID {
		return
	}
	current.Expires = c.now().Add(c.settings.SessionAffinityTTL)
	c.bindings[sessionKey] = current
}

// ReportFailure cools an account down and may release its session binding.
func (c *Conductor) ReportFailure(sessionKey, accountID string, class FailureClass) {
	if c == nil || accountID == "" || class == "" || class == FailurePermanent {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.settings.DisableCooling {
		c.cooldown[accountID] = c.now().Add(cooldownFor(class))
	}
	if sessionKey == "" || !c.settings.SessionAffinity {
		return
	}
	release := c.settings.RebindOn == RebindAny || class == FailureQuota || class == FailureAuth
	if release {
		delete(c.bindings, sessionKey)
	}
}

// Cooldowns returns the in-memory cooldown deadlines. The map is a copy.
func (c *Conductor) Cooldowns() map[string]time.Time {
	if c == nil {
		return map[string]time.Time{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]time.Time, len(c.cooldown))
	for id, until := range c.cooldown {
		out[id] = until
	}
	return out
}

// ResetCooldown clears one account, or every account when id is empty.
func (c *Conductor) ResetCooldown(id string) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if id == "" {
		c.cooldown = map[string]time.Time{}
		return
	}
	delete(c.cooldown, id)
}

func (c *Conductor) candidate(req SelectRequest, consume bool) (Account, time.Duration, bool) {
	now := c.now()
	c.expire(now)
	if c.settings.MaxRetryCredentials > 0 && len(req.Tried) >= c.settings.MaxRetryCredentials {
		return Account{}, 0, false
	}
	eligible := c.eligible(req, now)
	if len(eligible) == 0 {
		return Account{}, c.soonest(now), false
	}
	if account, ok := c.sticky(req, eligible); ok {
		return account, 0, true
	}
	account, ok := c.byStrategy(eligible, consume)
	return account, 0, ok
}

func (c *Conductor) eligible(req SelectRequest, now time.Time) []Account {
	out := make([]Account, 0, len(c.accounts))
	for _, account := range c.accounts {
		if !c.admits(account, req, now) {
			continue
		}
		out = append(out, account)
	}
	return out
}

func (c *Conductor) admits(account Account, req SelectRequest, now time.Time) bool {
	if !account.Enabled || account.ID == "" || account.Provider == "" {
		return false
	}
	if req.Provider != "" && account.Provider != req.Provider {
		return false
	}
	if slices.Contains(req.Tried, account.ID) {
		return false
	}
	if until, cooling := c.cooldown[account.ID]; cooling && !until.After(now) {
		delete(c.cooldown, account.ID)
	} else if cooling {
		return false
	}
	retry := c.settings.RequestRetry
	if account.RequestRetryConfigured {
		retry = account.RequestRetry
	}
	if req.Round > retry {
		return false
	}
	if c.settings.Strategy == StrategyWeighted {
		if account.Weight <= 0 || account.Weight > MaxWeight {
			return false
		}
	}
	if !modelAllowed(account, req.ModelID, c.settings.ForceModelPrefix) {
		return false
	}
	return true
}

func modelAllowed(account Account, modelID string, forcePrefix bool) bool {
	if modelID == "" {
		return false
	}
	for _, excluded := range account.ExcludedModels {
		if excluded == modelID {
			return false
		}
	}
	if account.Prefix != "" && account.Prefix != modelID && !strings.HasPrefix(modelID, account.Prefix+"/") {
		return false
	}
	if forcePrefix && account.Prefix != "" && !strings.Contains(modelID, "/") && account.Prefix != modelID {
		return false
	}
	return true
}

func (c *Conductor) sticky(req SelectRequest, eligible []Account) (Account, bool) {
	if !c.settings.SessionAffinity {
		return Account{}, false
	}
	if c.settings.Subagents && req.ParentSessionKey != "" {
		if parent, ok := c.bindings[req.ParentSessionKey]; ok {
			if account, found := findAccount(eligible, parent.AccountID); found && (parent.ModelID == req.ModelID || req.Guardian) {
				return account, true
			}
		}
	}
	if req.SessionKey == "" {
		return Account{}, false
	}
	current, ok := c.bindings[req.SessionKey]
	if !ok {
		return Account{}, false
	}
	return findAccount(eligible, current.AccountID)
}

func (c *Conductor) byStrategy(eligible []Account, consume bool) (Account, bool) {
	if len(eligible) == 0 {
		return Account{}, false
	}
	switch c.settings.Strategy {
	case StrategyFillFirst:
		return eligible[0], true
	case StrategyWeighted:
		return c.weighted(eligible, consume)
	default:
		return c.roundRobin(eligible, consume)
	}
}

func (c *Conductor) roundRobin(eligible []Account, consume bool) (Account, bool) {
	order := c.accounts
	start := c.cursor
	for offset := range len(order) {
		account := order[(start+offset)%len(order)]
		if !containsID(eligible, account.ID) {
			continue
		}
		if consume {
			c.cursor = (start + offset + 1) % len(order)
		}
		return account, true
	}
	return Account{}, false
}

func (c *Conductor) weighted(eligible []Account, consume bool) (Account, bool) {
	total := 0
	for _, account := range eligible {
		total += account.Weight
	}
	if total == 0 {
		return Account{}, false
	}
	index := c.cursor % total
	if consume {
		c.cursor++
	}
	for _, account := range eligible {
		if index < account.Weight {
			return account, true
		}
		index -= account.Weight
	}
	return eligible[0], true
}

func (c *Conductor) bind(req SelectRequest, account Account) {
	if !c.settings.SessionAffinity || req.SessionKey == "" {
		return
	}
	// A failover attempt must not steal a binding that ReportFailure left in place.
	if current, ok := c.bindings[req.SessionKey]; ok && current.Expires.After(c.now()) {
		return
	}
	c.bindings[req.SessionKey] = binding{
		AccountID: account.ID,
		ModelID:   req.ModelID,
		Expires:   c.now().Add(c.settings.SessionAffinityTTL),
	}
}

func (c *Conductor) expire(now time.Time) {
	for key, current := range c.bindings {
		if !current.Expires.After(now) {
			delete(c.bindings, key)
		}
	}
}

func (c *Conductor) soonest(now time.Time) time.Duration {
	var wait time.Duration
	for _, until := range c.cooldown {
		if !until.After(now) {
			continue
		}
		remaining := until.Sub(now)
		if wait == 0 || remaining < wait {
			wait = remaining
		}
	}
	return wait
}

func findAccount(accounts []Account, id string) (Account, bool) {
	for _, account := range accounts {
		if account.ID == id {
			return account, true
		}
	}
	return Account{}, false
}

func containsID(accounts []Account, id string) bool {
	for _, account := range accounts {
		if account.ID == id {
			return true
		}
	}
	return false
}

func cooldownFor(class FailureClass) time.Duration {
	switch class {
	case FailureAuth:
		return DefaultAuthCooldown
	case FailureQuota:
		return DefaultQuotaCooldown
	default:
		return DefaultTransientCooldown
	}
}
