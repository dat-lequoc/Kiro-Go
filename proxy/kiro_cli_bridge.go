// Package proxy: kiro-cli bridge for Kiro Secret Key (ksk_) accounts.
//
// Kiro Secret Keys (ksk_...) are long-lived API keys that Kiro issues for
// headless `kiro-cli` use. Unlike OAuth accounts, a ksk_ key is NOT a usable
// bearer token on its own: the official client exchanges it via AWS SSO-OIDC
// for a short-lived (~15 min) token before calling the CodeWhisperer/Q data
// plane. That exchange is undocumented and the kiro-cli binary pins its TLS
// trust store, so it cannot be reproduced natively here.
//
// This bridge therefore drives the installed `kiro-cli` binary as a subprocess
// with KIRO_API_KEY set, capturing its stdout as the model response. It plugs
// into the same CallKiroAPI/KiroStreamCallback seam every other auth method
// uses, so streaming, token accounting and failover behave consistently.
//
// See docs/kiro-secret-key-ksk.md for the full research write-up.
package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"kiro-go/config"
	"kiro-go/logger"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"time"
)

const (
	// defaultKiroCliBinary is used when KIRO_CLI_PATH is not set.
	defaultKiroCliBinary = "kiro-cli"
	// bridgeTimeout caps a single kiro-cli invocation.
	bridgeTimeout = 5 * time.Minute
)

// ansiBridgeRe strips ANSI escape sequences emitted by kiro-cli.
var ansiBridgeRe = regexp.MustCompile(`\x1b\[[0-9;?]*[a-zA-Z]`)

// creditsBridgeRe extracts the credit figure from the kiro-cli status line,
// e.g. " ▸ Credits: 0.05 • Time: 1s".
var creditsBridgeRe = regexp.MustCompile(`Credits:\s*([0-9]+(?:\.[0-9]+)?)`)

// kiroCliBinaryPath returns the kiro-cli executable to invoke. Operators can
// override the location with the KIRO_CLI_PATH environment variable.
func kiroCliBinaryPath() string {
	if p := strings.TrimSpace(os.Getenv("KIRO_CLI_PATH")); p != "" {
		return p
	}
	return defaultKiroCliBinary
}

// isApiKeyAccount reports whether the account authenticates via a Kiro Secret
// Key driven through the kiro-cli bridge.
func isApiKeyAccount(account *config.Account) bool {
	return account != nil && strings.EqualFold(account.AuthMethod, "apikey")
}

// callKiroViaCliBridge runs kiro-cli for an apikey account and feeds the
// captured response back through the standard streaming callback.
func callKiroViaCliBridge(account *config.Account, payload *KiroPayload, callback *KiroStreamCallback) error {
	apiKey := strings.TrimSpace(account.ApiKey)
	if apiKey == "" {
		return fmt.Errorf("apikey account %s has an empty Kiro Secret Key", account.ID)
	}

	prompt := buildBridgePrompt(payload)
	if strings.TrimSpace(prompt) == "" {
		prompt = minimalFallbackUserContent
	}
	model := strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.ModelID)

	stdout, stderr, runErr := runKiroCli(apiKey, prompt, model, bridgeTimeout)
	if runErr != nil {
		return classifyBridgeError(runErr, stderr)
	}
	// kiro-cli can exit 0 while still reporting an auth/quota failure on stderr
	// (e.g. a rejected Kiro Secret Key). Detect that here so the failure reaches
	// the failover layer instead of surfacing as an empty "successful" reply.
	if failure := detectBridgeStderrFailure(stderr); failure != nil {
		return failure
	}

	text := cleanBridgeOutput(stdout)
	credits := parseBridgeCredits(stderr)

	if callback != nil {
		if callback.OnText != nil && text != "" {
			callback.OnText(text, false)
		}
		if callback.OnCredits != nil && credits > 0 {
			callback.OnCredits(credits)
		}
		if callback.OnComplete != nil {
			// Pass zero token counts; the handler estimates input/output from
			// the request and accumulated content when these are unset.
			callback.OnComplete(0, 0)
		}
	}
	return nil
}

// runKiroCli invokes the kiro-cli binary once, piping prompt to stdin and
// returning stdout, stderr and any process error.
func runKiroCli(apiKey, prompt, model string, timeout time.Duration) (string, string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	args := []string{"chat", "--no-interactive", "--trust-tools="}
	if model != "" {
		args = append(args, "--model", model)
	}

	cmd := exec.CommandContext(ctx, kiroCliBinaryPath(), args...)
	cmd.Env = append(os.Environ(),
		"KIRO_API_KEY="+apiKey,
		"NO_COLOR=1",
		"KIRO_DISABLE_TELEMETRY=1",
	)
	cmd.Stdin = strings.NewReader(prompt)

	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() == context.DeadlineExceeded {
		return stdout.String(), stderr.String(), fmt.Errorf("kiro-cli bridge timed out after %s", timeout)
	}
	return stdout.String(), stderr.String(), err
}

// buildBridgePrompt reconstructs a plain-text prompt from the converted Kiro
// payload. History turns are rendered as a labeled transcript so multi-turn
// context is preserved; the final user message is appended verbatim.
//
// Note: structured tool calls and images cannot be forwarded through the
// kiro-cli stdin interface and are omitted (text content is preserved).
func buildBridgePrompt(payload *KiroPayload) string {
	if payload == nil {
		return ""
	}
	var b strings.Builder
	for _, h := range payload.ConversationState.History {
		if h.UserInputMessage != nil {
			if c := strings.TrimSpace(h.UserInputMessage.Content); c != "" {
				b.WriteString("User: ")
				b.WriteString(c)
				b.WriteString("\n\n")
			}
		}
		if h.AssistantResponseMessage != nil {
			if c := strings.TrimSpace(h.AssistantResponseMessage.Content); c != "" {
				b.WriteString("Assistant: ")
				b.WriteString(c)
				b.WriteString("\n\n")
			}
		}
	}
	current := strings.TrimSpace(payload.ConversationState.CurrentMessage.UserInputMessage.Content)
	if b.Len() > 0 && current != "" {
		b.WriteString("User: ")
	}
	b.WriteString(current)
	return strings.TrimSpace(b.String())
}

// cleanBridgeOutput removes ANSI codes, the leading "> " response marker, and
// any stray credit/spinner lines from kiro-cli stdout.
func cleanBridgeOutput(raw string) string {
	out := ansiBridgeRe.ReplaceAllString(raw, "")
	out = strings.ReplaceAll(out, "\r", "")
	lines := strings.Split(out, "\n")
	kept := make([]string, 0, len(lines))
	for i, line := range lines {
		trimmed := strings.TrimSpace(line)
		// Drop the trailing "▸ Credits: ... • Time: ..." status line if it
		// leaks into stdout on some terminals.
		if creditsBridgeRe.MatchString(trimmed) && strings.Contains(trimmed, "Time:") {
			continue
		}
		if i == 0 {
			line = strings.TrimPrefix(line, "> ")
		}
		kept = append(kept, line)
	}
	return strings.TrimSpace(strings.Join(kept, "\n"))
}

// parseBridgeCredits pulls the credit figure from the kiro-cli status line.
func parseBridgeCredits(stderr string) float64 {
	clean := ansiBridgeRe.ReplaceAllString(stderr, "")
	m := creditsBridgeRe.FindStringSubmatch(clean)
	if len(m) < 2 {
		return 0
	}
	v, err := strconv.ParseFloat(m[1], 64)
	if err != nil {
		return 0
	}
	return v
}

// bridgeTextIndicatesAuthFailure reports whether kiro-cli output signals an
// authentication problem. kiro-cli sometimes exits 0 even when the Kiro Secret
// Key is rejected, printing the failure to stderr instead, so callers must scan
// the text directly rather than relying solely on the process exit code.
func bridgeTextIndicatesAuthFailure(lower string) bool {
	return strings.Contains(lower, "bearer token") ||
		strings.Contains(lower, "accessdenied") ||
		strings.Contains(lower, "unauthorized") ||
		strings.Contains(lower, "invalid api key") ||
		strings.Contains(lower, "forbidden") ||
		strings.Contains(lower, "authentication failed") ||
		strings.Contains(lower, "api key may be invalid")
}

// bridgeTextIndicatesQuota reports whether kiro-cli output signals a quota or
// rate-limit condition.
func bridgeTextIndicatesQuota(lower string) bool {
	return strings.Contains(lower, "quota") || strings.Contains(lower, "429")
}

// detectBridgeStderrFailure inspects kiro-cli stderr for auth/quota failures
// that the process reports without a non-zero exit code.
func detectBridgeStderrFailure(stderr string) error {
	lower := strings.ToLower(ansiBridgeRe.ReplaceAllString(stderr, ""))
	switch {
	case bridgeTextIndicatesAuthFailure(lower):
		return classifyBridgeError(fmt.Errorf("kiro-cli reported auth failure"), stderr)
	case bridgeTextIndicatesQuota(lower):
		return classifyBridgeError(fmt.Errorf("kiro-cli reported quota failure"), stderr)
	}
	return nil
}

// classifyBridgeError converts a kiro-cli failure into an error whose message
// the failover layer can classify (auth vs transient). The stderr tail is
// included for operator visibility.
func classifyBridgeError(runErr error, stderr string) error {
	tail := strings.TrimSpace(ansiBridgeRe.ReplaceAllString(stderr, ""))
	if len(tail) > 600 {
		tail = tail[len(tail)-600:]
	}
	lower := strings.ToLower(tail)
	switch {
	case bridgeTextIndicatesAuthFailure(lower):
		return fmt.Errorf("kiro-cli bridge auth error (http 403): %s", tail)
	case bridgeTextIndicatesQuota(lower):
		return fmt.Errorf("kiro-cli bridge quota error (429): %s", tail)
	}
	if tail == "" {
		return fmt.Errorf("kiro-cli bridge failed: %v", runErr)
	}
	return fmt.Errorf("kiro-cli bridge failed: %v: %s", runErr, tail)
}

// bridgeListModels fetches the model catalog for an apikey account by invoking
// `kiro-cli chat --list-models --format json`.
func bridgeListModels(account *config.Account) ([]ModelInfo, error) {
	apiKey := strings.TrimSpace(account.ApiKey)
	if apiKey == "" {
		return nil, fmt.Errorf("apikey account %s has an empty Kiro Secret Key", account.ID)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	cmd := exec.CommandContext(ctx, kiroCliBinaryPath(),
		"chat", "--list-models", "--format", "json")
	cmd.Env = append(os.Environ(),
		"KIRO_API_KEY="+apiKey,
		"NO_COLOR=1",
		"KIRO_DISABLE_TELEMETRY=1",
	)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if err := cmd.Run(); err != nil {
		if ctx.Err() == context.DeadlineExceeded {
			return nil, fmt.Errorf("kiro-cli list-models timed out")
		}
		return nil, classifyBridgeError(err, stderr.String())
	}

	models, err := parseBridgeModels(stdout.Bytes())
	if err != nil {
		return nil, err
	}
	if len(models) == 0 {
		// A rejected key yields only the static "auto" fallback (which we skip),
		// so an empty catalog means the Kiro Secret Key did not authenticate.
		return nil, fmt.Errorf("kiro-cli bridge auth error (http 403): no models returned; the Kiro Secret Key may be invalid or expired")
	}
	logger.Debugf("[KiroCLIBridge] Listed %d models for apikey account %s", len(models), account.ID)
	return models, nil
}

// parseBridgeModels converts the `kiro-cli --list-models --format json` output
// into the proxy ModelInfo shape. The CLI JSON uses snake_case fields that
// differ from the CodeWhisperer REST shape, so it is mapped explicitly.
func parseBridgeModels(data []byte) ([]ModelInfo, error) {
	var raw struct {
		Models []struct {
			ModelName           string  `json:"model_name"`
			ModelID             string  `json:"model_id"`
			Description         string  `json:"description"`
			ContextWindowTokens int     `json:"context_window_tokens"`
			RateMultiplier      float64 `json:"rate_multiplier"`
		} `json:"models"`
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("parse kiro-cli models: %w", err)
	}
	models := make([]ModelInfo, 0, len(raw.Models))
	for _, m := range raw.Models {
		id := strings.TrimSpace(m.ModelID)
		if id == "" {
			continue
		}
		// "auto" is a CLI routing alias, not a concrete servable model id.
		if strings.EqualFold(id, "auto") {
			continue
		}
		info := ModelInfo{
			ModelId:        id,
			ModelName:      m.ModelName,
			Description:    m.Description,
			InputTypes:     []string{"text"},
			RateMultiplier: m.RateMultiplier,
		}
		models = append(models, info)
	}
	return models, nil
}

// listAccountModels returns the model catalog for an account, dispatching to
// the kiro-cli bridge for apikey accounts and the CodeWhisperer REST API
// otherwise.
func listAccountModels(account *config.Account) ([]ModelInfo, error) {
	if isApiKeyAccount(account) {
		return bridgeListModels(account)
	}
	return ListAvailableModels(account)
}
