package proxy

import (
	"kiro-go/config"
	"kiro-go/pool"
	"path/filepath"
	"testing"
	"time"
)

func TestAccountFailureClassifiers(t *testing.T) {
	tests := []struct {
		name string
		fn   func(string) bool
		msg  string
	}{
		{name: "quota", fn: isQuotaErrorMessage, msg: "HTTP 429: quota exhausted"},
		{name: "overage", fn: isOverageErrorMessage, msg: "HTTP 402 from Kiro IDE: OVERAGE limit exceeded"},
		{name: "suspension", fn: isSuspensionErrorMessage, msg: "Your User ID temporarily is suspended"},
		{name: "profile", fn: isProfileUnavailableErrorMessage, msg: "no available Kiro profile"},
		{name: "auth", fn: isAuthErrorMessage, msg: "Authentication failed - token invalid or expired"},
	}

	for _, tc := range tests {
		if !tc.fn(tc.msg) {
			t.Fatalf("%s classifier did not match %q", tc.name, tc.msg)
		}
	}
}

func TestQuotaFailureUsesRetryAfterFromMessage(t *testing.T) {
	msg := "HTTP 429: quota exhausted, retry-after: 12"
	if got := pool.ParseRetryAfter(msg); got != 12*time.Second {
		t.Fatalf("expected 12s retry-after, got %v", got)
	}
}

func TestHandleAuthFailureDoesNotDisableOnFirstFailure(t *testing.T) {
	cfgFile := filepath.Join(t.TempDir(), "config.json")
	if err := config.Init(cfgFile); err != nil {
		t.Fatalf("config.Init: %v", err)
	}
	account := config.Account{
		ID:           "auth-test",
		Enabled:      true,
		Email:        "auth@test",
		RefreshToken: "invalid-refresh-token",
	}
	if err := config.AddAccount(account); err != nil {
		t.Fatalf("AddAccount: %v", err)
	}

	h := &Handler{pool: pool.GetPool()}
	h.pool.Reload()
	h.handleAuthFailure(&account, errAuthTest)

	got := config.GetAccounts()
	if len(got) != 1 || !got[0].Enabled {
		t.Fatalf("expected account to remain enabled after first auth failure, got %#v", got)
	}
}

var errAuthTest = &authTestError{msg: "HTTP 401: unauthorized"}

type authTestError struct{ msg string }

func (e *authTestError) Error() string { return e.msg }
