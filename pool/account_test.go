package pool

import (
	"errors"
	"kiro-go/config"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestOverLimitAccountsCanBeSelectedByDefault(t *testing.T) {
	p := &AccountPool{}
	normal := config.Account{ID: "normal"}
	overLimit := config.Account{ID: "over", UsageCurrent: 10, UsageLimit: 10}

	p.accounts = []config.Account{normal, overLimit}

	seenOverLimit := false
	for i := 0; i < 5; i++ {
		acc := p.GetNext()
		if acc == nil {
			t.Fatalf("expected an account")
		}
		if acc.ID == "over" {
			seenOverLimit = true
		}
	}
	if !seenOverLimit {
		t.Fatalf("expected over-limit account to remain selectable when upstream OverageStatus is empty")
	}
}

func TestOverLimitAccountsCanBeSelectedWhenUpstreamOverageEnabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "ENABLED",
	}

	p.accounts = []config.Account{overLimit}

	acc := p.GetNext()
	if acc == nil {
		t.Fatalf("expected upstream-enabled overage account to be selectable")
	}
	if acc.ID != "over" {
		t.Fatalf("expected overage account, got %q", acc.ID)
	}
}

func TestOverLimitAccountsRemainSelectableWhenUpstreamOverageDisabled(t *testing.T) {
	p := &AccountPool{}
	overLimit := config.Account{
		ID:            "over",
		UsageCurrent:  10,
		UsageLimit:    10,
		OverageStatus: "DISABLED",
	}

	p.accounts = []config.Account{overLimit}

	if acc := p.GetNext(); acc == nil || acc.ID != "over" {
		t.Fatalf("expected over-limit account to remain selectable with hard-coded over-usage, got %#v", acc)
	}
}

func TestGetNextKeepsFiveMinuteTokenAvailable(t *testing.T) {
	p := &AccountPool{}
	account := config.Account{
		ID:          "acct-1",
		AccessToken: "access-token",
		ExpiresAt:   time.Now().Unix() + 300,
	}

	p.accounts = []config.Account{account}

	got := p.GetNext()
	if got == nil {
		t.Fatalf("expected five-minute token to be available")
	}
	if got.ID != account.ID {
		t.Fatalf("expected account %q, got %q", account.ID, got.ID)
	}
}

// ---------------------------------------------------------------------------
// IsAuthFailure
// ---------------------------------------------------------------------------

func TestIsAuthFailureRecognizes401And403(t *testing.T) {
	positives := []string{
		"HTTP 401 from server",
		"received 403 Forbidden",
		"bad credentials",
		"invalid_grant",
		"invalid_token",
		"token expired",
		"token has expired",
		"unauthorized",
	}
	for _, msg := range positives {
		if !IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = false, want true", msg)
		}
	}
}

func TestIsAuthFailureIgnoresFalsePositives(t *testing.T) {
	// hasStatusToken only excludes digit boundaries; e.g. "4011" contains "401"
	// but the trailing '1' is a digit so it does NOT match.
	negatives := []string{
		"status code 4011 found", // digit immediately after 401 → not a standalone token
		"error 14013 exceeded",   // digit before and after 401
		"some random error",
		"status 200 OK",
	}
	for _, msg := range negatives {
		if IsAuthFailure(errors.New(msg)) {
			t.Errorf("IsAuthFailure(%q) = true, want false", msg)
		}
	}
}

func TestIsAuthFailureNilError(t *testing.T) {
	if IsAuthFailure(nil) {
		t.Fatal("IsAuthFailure(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// IsSuspensionError
// ---------------------------------------------------------------------------

func TestIsSuspensionErrorDetectsKnownMessages(t *testing.T) {
	positives := []string{
		"account temporarily_suspended",
		"account temporarily suspended",
		"no available kiro profile",
		"No Available Kiro Profile", // case-insensitive
	}
	for _, msg := range positives {
		if !IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = false, want true", msg)
		}
	}
}

func TestIsSuspensionErrorIgnoresUnrelatedErrors(t *testing.T) {
	negatives := []string{
		"some other error",
		"unauthorized",
		"429 too many requests",
	}
	for _, msg := range negatives {
		if IsSuspensionError(errors.New(msg)) {
			t.Errorf("IsSuspensionError(%q) = true, want false", msg)
		}
	}
}

func TestIsSuspensionErrorNilError(t *testing.T) {
	if IsSuspensionError(nil) {
		t.Fatal("IsSuspensionError(nil) = true, want false")
	}
}

// ---------------------------------------------------------------------------
// GetNextForModelExcluding
// ---------------------------------------------------------------------------

func newTestPool(accounts ...config.Account) *AccountPool {
	p := &AccountPool{
		cooldowns:         make(map[string]time.Time),
		errorCounts:       make(map[string]int),
		authFailureCounts: make(map[string]int),
		modelLists:        make(map[string]map[string]bool),
		affinity:          make(map[string]affinityBinding),
	}
	p.accounts = accounts
	return p
}

func TestGetNextForModelExcludingSkipsExcludedAccounts(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	excluded := map[string]bool{"a": true}
	for i := 0; i < 5; i++ {
		acc := p.GetNextForModelExcluding("model", excluded)
		if acc == nil {
			t.Fatal("expected account b, got nil")
		}
		if acc.ID == "a" {
			t.Fatalf("excluded account a was returned on iteration %d", i)
		}
	}
}

func TestGetNextForModelExcludingReturnsNilWhenAllExcluded(t *testing.T) {
	p := newTestPool(config.Account{ID: "only"})
	acc := p.GetNextForModelExcluding("model", map[string]bool{"only": true})
	if acc != nil {
		t.Fatalf("expected nil when only account is excluded, got %q", acc.ID)
	}
}

func TestGetNextForModelExcludingReturnsNilOnEmptyPool(t *testing.T) {
	p := newTestPool()
	acc := p.GetNextForModelExcluding("model", map[string]bool{})
	if acc != nil {
		t.Fatalf("expected nil for empty pool, got %q", acc.ID)
	}
}

func TestGetNextForModelWithAffinityReturnsBoundAccount(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	p.RecordAffinitySuccess("conversation:1", "b")

	got := p.GetNextForModelWithAffinity("model", "conversation:1", nil)
	if got == nil {
		t.Fatal("expected bound account")
	}
	if got.ID != "b" {
		t.Fatalf("expected bound account b, got %q", got.ID)
	}
}

func TestGetNextForModelWithAffinitySkipsExcludedBoundAccount(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	p.RecordAffinitySuccess("conversation:1", "b")

	got := p.GetNextForModelWithAffinity("model", "conversation:1", map[string]bool{"b": true})
	if got == nil {
		t.Fatal("expected fallback account")
	}
	if got.ID == "b" {
		t.Fatalf("expected excluded bound account to be skipped, got %q", got.ID)
	}
}

func TestGetNextForModelWithAffinitySkipsCoolingBoundAccount(t *testing.T) {
	p := newTestPool(
		config.Account{ID: "a"},
		config.Account{ID: "b"},
	)
	p.RecordAffinitySuccess("conversation:1", "b")
	p.cooldowns["b"] = time.Now().Add(time.Minute)

	got := p.GetNextForModelWithAffinity("model", "conversation:1", nil)
	if got == nil {
		t.Fatal("expected fallback account")
	}
	if got.ID == "b" {
		t.Fatalf("expected cooling bound account to be skipped, got %q", got.ID)
	}
}

// ---------------------------------------------------------------------------
// DisableAccount
// ---------------------------------------------------------------------------

func TestDisableAccountSetsCooldown(t *testing.T) {
	// Initialize a temporary config so SetAccountBanStatus can persist safely.
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}

	p := newTestPool()
	p.DisableAccount("test-id", "test reason")

	p.mu.RLock()
	cooldown, ok := p.cooldowns["test-id"]
	p.mu.RUnlock()

	if !ok {
		t.Fatal("expected cooldown to be set after DisableAccount")
	}
	// Safety-net cooldown must be at least 23 hours from now.
	minExpected := time.Now().Add(23 * time.Hour)
	if cooldown.Before(minExpected) {
		t.Fatalf("expected cooldown >= 23h in future, got %v", cooldown)
	}
}

func TestGetNextExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}

	acc := p.GetNextExcluding(map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

func TestGetNextForModelExcludingSkipsExcludedAccount(t *testing.T) {
	p := &AccountPool{
		accounts: []config.Account{
			{ID: "a", Enabled: true},
			{ID: "b", Enabled: true},
		},
		cooldowns:    make(map[string]time.Time),
		errorCounts:  make(map[string]int),
		modelLists:   make(map[string]map[string]bool),
		currentIndex: ^uint64(0),
	}
	p.SetModelList("a", []string{"claude-sonnet-4.5"})
	p.SetModelList("b", []string{"claude-sonnet-4.5"})

	acc := p.GetNextForModelExcluding("claude-sonnet-4.5", map[string]bool{"a": true})
	if acc == nil || acc.ID != "b" {
		t.Fatalf("expected account b, got %#v", acc)
	}
}

// ---------------------------------------------------------------------------
// Reload over-usage filtering
// ---------------------------------------------------------------------------

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsage(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(true); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable when allowOverUsage=true, got %#v", got)
	}
}

func TestReloadKeepsOverQuotaAccountWhenAllowOverUsageSavedDisabled(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{
		ID:           "over",
		Enabled:      true,
		UsageCurrent: 10,
		UsageLimit:   10,
	}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	if err := config.UpdateAllowOverUsage(false); err != nil {
		t.Fatalf("UpdateAllowOverUsage: %v", err)
	}

	p := newTestPool()
	p.Reload()

	if got := p.GetNext(); got == nil || got.ID != "over" {
		t.Fatalf("expected over-quota account to remain routable with hard-coded over-usage, got %#v", got)
	}
}

// ---------------------------------------------------------------------------
// Quota backoff / retry-after
// ---------------------------------------------------------------------------

func TestRecordErrorQuotaUsesExponentialBackoff(t *testing.T) {
	p := newTestPool(config.Account{ID: "a"})

	expected := []time.Duration{5 * time.Second, 10 * time.Second, 20 * time.Second, 20 * time.Second}
	for i, want := range expected {
		before := time.Now()
		p.RecordError("a", true, 0)
		p.mu.RLock()
		cooldown := p.cooldowns["a"]
		p.mu.RUnlock()
		if got := cooldown.Sub(before).Round(time.Second); got != want {
			t.Fatalf("attempt %d: expected backoff %s, got %s", i+1, want, got)
		}
	}
}

func TestRecordErrorQuotaHonorsRetryAfter(t *testing.T) {
	p := newTestPool(config.Account{ID: "a"})

	before := time.Now()
	p.RecordError("a", true, 45*time.Second)
	p.mu.RLock()
	cooldown := p.cooldowns["a"]
	p.mu.RUnlock()

	if got := cooldown.Sub(before).Round(time.Second); got != 45*time.Second {
		t.Fatalf("expected retry-after 45s to win over default backoff, got %s", got)
	}
}

func TestParseRetryAfter(t *testing.T) {
	cases := []struct {
		msg  string
		want time.Duration
	}{
		{"HTTP 429 retry-after: 30", 30 * time.Second},
		{"please retry after 2 minutes", 2 * time.Minute},
		{"Retry-After=1h", time.Hour},
		{"no hint here", 0},
		{"retry-after: 0", 0},
	}
	for _, tc := range cases {
		if got := ParseRetryAfter(tc.msg); got != tc.want {
			t.Errorf("ParseRetryAfter(%q) = %s, want %s", tc.msg, got, tc.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Auth failure counting
// ---------------------------------------------------------------------------

func TestRecordAuthErrorCountsAndCoolsDown(t *testing.T) {
	p := newTestPool(config.Account{ID: "a"})

	before := time.Now()
	if n := p.RecordAuthError("a"); n != 1 {
		t.Fatalf("expected first auth failure count 1, got %d", n)
	}
	if n := p.RecordAuthError("a"); n != 2 {
		t.Fatalf("expected second auth failure count 2, got %d", n)
	}

	p.mu.RLock()
	cooldown := p.cooldowns["a"]
	p.mu.RUnlock()
	if cooldown.Sub(before) < 29*time.Minute {
		t.Fatalf("expected ~30m auth cooldown, got %s", cooldown.Sub(before))
	}

	p.RecordAuthSuccess("a")
	if n := p.RecordAuthError("a"); n != 1 {
		t.Fatalf("expected count reset after RecordAuthSuccess, got %d", n)
	}
}

func TestRecordSuccessClearsAuthFailures(t *testing.T) {
	p := newTestPool(config.Account{ID: "a"})
	p.RecordAuthError("a")
	p.RecordSuccess("a")
	if n := p.RecordAuthError("a"); n != 1 {
		t.Fatalf("expected auth failure count reset after RecordSuccess, got %d", n)
	}
}

// ---------------------------------------------------------------------------
// UnavailableReason diagnostics
// ---------------------------------------------------------------------------

func TestUnavailableReasonNoAccountsConfigured(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	p := newTestPool()
	if got := p.UnavailableReason("claude-opus-4.8"); got != "No accounts configured" {
		t.Fatalf("expected 'No accounts configured', got %q", got)
	}
}

func TestUnavailableReasonModelUnsupported(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "a", Enabled: true, Email: "a@test"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := newTestPool()
	p.Reload()
	p.SetModelList("a", []string{"claude-opus-4.7"})

	got := p.UnavailableReason("claude-opus-4.8")
	if !strings.Contains(got, "No account supports model") {
		t.Fatalf("expected unsupported-model message, got %q", got)
	}
	if !strings.Contains(got, "claude-opus-4.7") {
		t.Fatalf("expected alternative model suggestion, got %q", got)
	}
}

func TestUnavailableReasonAllCooling(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	if err := config.AddAccount(config.Account{ID: "a", Enabled: true, Email: "a@test"}); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}
	p := newTestPool()
	p.Reload()
	p.mu.Lock()
	p.cooldowns["a"] = time.Now().Add(30 * time.Second)
	p.mu.Unlock()

	got := p.UnavailableReason("claude-opus-4.8")
	if !strings.Contains(got, "retry in") {
		t.Fatalf("expected cooldown retry hint, got %q", got)
	}
}
