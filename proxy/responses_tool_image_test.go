package proxy

import (
	"encoding/json"
	"strings"
	"testing"
)

// tinyPNGDataURL is a 1x1 transparent PNG as a data URL, standing in for a
// view_image tool result.
const tinyPNGDataURL = "data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNkYPhfDwAChwGA60e6kgAAAABJRU5ErkJggg=="

// TestResponsesToolOutputImagePreserved verifies that a function_call_output
// whose output carries an input_image part is preserved as structured content
// (so the image is forwarded as vision input) instead of being JSON-stringified
// into plain text. The latter caused a 512x512 icon to burn ~12.8k tokens
// instead of ~350 because the base64 data-URI was counted (and duplicated) as
// text. See docs/opus-image-token-bug-20260611.md.
func TestResponsesToolOutputImagePreserved(t *testing.T) {
	output := `[{"type":"input_image","image_url":"` + tinyPNGDataURL + `"}]`
	items := []json.RawMessage{
		mustRaw(`{"type":"message","role":"user","content":[{"type":"input_text","text":"view it"}]}`),
		mustRaw(`{"type":"function_call","call_id":"call_a","name":"view_image","arguments":"{\"path\":\"x.png\"}"}`),
		mustRaw(`{"type":"function_call_output","call_id":"call_a","output":` + output + `}`),
	}

	msgs, err := convertResponsesInputItems(items)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}

	var tool *OpenAIMessage
	for i := range msgs {
		if msgs[i].Role == "tool" {
			tool = &msgs[i]
			break
		}
	}
	if tool == nil {
		t.Fatal("no tool message produced")
	}

	// Content must be structured parts, not a flattened string blob.
	if s, ok := tool.Content.(string); ok {
		t.Fatalf("tool content was stringified (len=%d); image lost to text", len(s))
	}

	// Downstream extraction must recover a real image, no base64 left as text.
	text, images := extractOpenAIUserContent(tool.Content)
	if len(images) != 1 {
		t.Fatalf("expected 1 extracted image, got %d", len(images))
	}
	if strings.Contains(text, "base64") || strings.Contains(text, "iVBOR") {
		t.Fatalf("base64 payload leaked into text content: %q", text)
	}
}

// TestResponsesToolOutputTextStillString verifies the common case (plain string
// tool output) is unchanged: it stays a string, not wrapped in parts.
func TestResponsesToolOutputTextStillString(t *testing.T) {
	items := []json.RawMessage{
		mustRaw(`{"type":"function_call_output","call_id":"c","output":"plain result"}`),
	}
	msgs, err := convertResponsesInputItems(items)
	if err != nil {
		t.Fatalf("convert: %v", err)
	}
	if len(msgs) != 1 || msgs[0].Role != "tool" {
		t.Fatalf("unexpected messages: %+v", msgs)
	}
	if s, ok := msgs[0].Content.(string); !ok || s != "plain result" {
		t.Fatalf("expected plain string content, got %#v", msgs[0].Content)
	}
}
