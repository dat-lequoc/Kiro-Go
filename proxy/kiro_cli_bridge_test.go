package proxy

import (
	"kiro-go/config"
	"strings"
	"testing"
)

func TestIsApiKeyAccount(t *testing.T) {
	if !isApiKeyAccount(&config.Account{AuthMethod: "apikey"}) {
		t.Fatal("expected apikey account to be detected")
	}
	if !isApiKeyAccount(&config.Account{AuthMethod: "ApiKey"}) {
		t.Fatal("expected case-insensitive match")
	}
	if isApiKeyAccount(&config.Account{AuthMethod: "idc"}) {
		t.Fatal("idc account must not be treated as apikey")
	}
	if isApiKeyAccount(nil) {
		t.Fatal("nil account must not be treated as apikey")
	}
}

func TestCleanBridgeOutputStripsMarkerAndAnsi(t *testing.T) {
	raw := "\x1b[m> \x1b[0mHello\x1b[0m\nWorld\x1b[0m"
	got := cleanBridgeOutput(raw)
	if got != "Hello\nWorld" {
		t.Fatalf("unexpected cleaned output: %q", got)
	}
}

func TestCleanBridgeOutputDropsCreditsLine(t *testing.T) {
	raw := "> Answer here\n \u25b8 Credits: 0.05 \u2022 Time: 1s\n"
	got := cleanBridgeOutput(raw)
	if got != "Answer here" {
		t.Fatalf("expected credits status line dropped, got %q", got)
	}
}

func TestParseBridgeCredits(t *testing.T) {
	stderr := "\x1b[m \u25b8 Credits: 0.11 \u2022 Time: 2s\x1b[0m"
	if got := parseBridgeCredits(stderr); got != 0.11 {
		t.Fatalf("expected 0.11 credits, got %v", got)
	}
	if got := parseBridgeCredits("no credits here"); got != 0 {
		t.Fatalf("expected 0 when no credits line, got %v", got)
	}
}

func TestBuildBridgePromptTranscript(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.History = []KiroHistoryMessage{
		{UserInputMessage: &KiroUserInputMessage{Content: "first question"}},
		{AssistantResponseMessage: &KiroAssistantResponseMessage{Content: "first answer"}},
	}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "second question"

	got := buildBridgePrompt(payload)
	for _, want := range []string{"User: first question", "Assistant: first answer", "User: second question"} {
		if !strings.Contains(got, want) {
			t.Fatalf("prompt missing %q, got:\n%s", want, got)
		}
	}
	if strings.Index(got, "first question") > strings.Index(got, "second question") {
		t.Fatal("history must precede current message")
	}
}

func TestBuildBridgePromptSingleTurn(t *testing.T) {
	payload := &KiroPayload{}
	payload.ConversationState.CurrentMessage.UserInputMessage.Content = "only message"
	got := buildBridgePrompt(payload)
	if got != "only message" {
		t.Fatalf("expected bare current message, got %q", got)
	}
}

func TestParseBridgeModelsSkipsAuto(t *testing.T) {
	data := []byte(`{"models":[
		{"model_name":"auto","model_id":"auto","rate_multiplier":1.0},
		{"model_name":"Claude Sonnet 4.5","model_id":"claude-sonnet-4.5","description":"d","rate_multiplier":1.3}
	],"default_model":"auto"}`)
	models, err := parseBridgeModels(data)
	if err != nil {
		t.Fatalf("parse failed: %v", err)
	}
	if len(models) != 1 {
		t.Fatalf("expected 1 model (auto skipped), got %d", len(models))
	}
	if models[0].ModelId != "claude-sonnet-4.5" {
		t.Fatalf("unexpected model id: %q", models[0].ModelId)
	}
	if models[0].RateMultiplier != 1.3 {
		t.Fatalf("rate multiplier not mapped: %v", models[0].RateMultiplier)
	}
}

func TestClassifyBridgeErrorAuth(t *testing.T) {
	err := classifyBridgeError(errStub("exit status 1"), "The bearer token included in the request is invalid.")
	if !isAuthErrorMessage(strings.ToLower(err.Error())) {
		t.Fatalf("expected auth-classifiable error, got %q", err.Error())
	}
}

func TestClassifyBridgeErrorQuota(t *testing.T) {
	err := classifyBridgeError(errStub("exit status 1"), "Error: quota exceeded (429)")
	if !isQuotaErrorMessage(err.Error()) {
		t.Fatalf("expected quota-classifiable error, got %q", err.Error())
	}
}

type errStub string

func (e errStub) Error() string { return string(e) }

func TestDetectBridgeStderrFailureAuth(t *testing.T) {
	// kiro-cli exits 0 but prints an auth failure to stderr for a rejected key.
	stderr := "\x1b[m\nAuthentication failed. Your API key may be invalid or expired. Check your KIRO_API_KEY value.\n"
	err := detectBridgeStderrFailure(stderr)
	if err == nil {
		t.Fatal("expected auth failure to be detected from stderr")
	}
	if !isAuthErrorMessage(strings.ToLower(err.Error())) {
		t.Fatalf("expected auth-classifiable error, got %q", err.Error())
	}
}

func TestDetectBridgeStderrFailureQuota(t *testing.T) {
	stderr := "Error: quota exceeded, please retry later (429)"
	err := detectBridgeStderrFailure(stderr)
	if err == nil {
		t.Fatal("expected quota failure to be detected from stderr")
	}
	if !isQuotaErrorMessage(err.Error()) {
		t.Fatalf("expected quota-classifiable error, got %q", err.Error())
	}
}

func TestDetectBridgeStderrFailureClean(t *testing.T) {
	// A normal warning-only stderr must not be treated as a failure.
	stderr := "WARNING: --trust-tools arg for custom tool needs to be prepended\n \u25b8 Credits: 0.05 \u2022 Time: 1s"
	if err := detectBridgeStderrFailure(stderr); err != nil {
		t.Fatalf("expected no failure for benign stderr, got %q", err.Error())
	}
}
