package netproxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestBuildRoundTripperRelayRewritesTargetHeaders(t *testing.T) {
	var sawTarget, sawPath string
	relay := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/relay" {
			t.Fatalf("relay path = %q, want /relay", r.URL.Path)
		}
		if r.URL.Query().Get("key") != "token" {
			t.Fatalf("relay key = %q, want token", r.URL.Query().Get("key"))
		}
		sawTarget = r.Header.Get("X-Relay-Target")
		sawPath = r.Header.Get("X-Relay-Path")
		_, _ = io.WriteString(w, "ok")
	}))
	defer relay.Close()

	rt, err := BuildRoundTripper("relay+"+relay.URL+"/relay?key=token", &http.Transport{})
	if err != nil {
		t.Fatalf("BuildRoundTripper: %v", err)
	}

	client := &http.Client{Transport: rt}
	resp, err := client.Get("https://q.us-east-1.amazonaws.com/generateAssistantResponse?x=1")
	if err != nil {
		t.Fatalf("client.Get: %v", err)
	}
	defer resp.Body.Close()

	if sawTarget != "https://q.us-east-1.amazonaws.com" {
		t.Fatalf("X-Relay-Target = %q", sawTarget)
	}
	if sawPath != "/generateAssistantResponse?x=1" {
		t.Fatalf("X-Relay-Path = %q", sawPath)
	}
}

func TestParseRelayURLAcceptsModalScheme(t *testing.T) {
	got, ok, err := ParseRelayURL("modal+https://relay.example.com/?key=x")
	if err != nil {
		t.Fatalf("ParseRelayURL: %v", err)
	}
	if !ok {
		t.Fatal("expected relay URL")
	}
	if got.String() != "https://relay.example.com/?key=x" {
		t.Fatalf("relay URL = %q", got.String())
	}
}
