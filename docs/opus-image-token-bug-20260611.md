# Opus image input burns ~30x tokens via Codex view_image (root cause + fix)

Date: 2026-06-11
Model: `claude-opus-4.8`
Symptom reported: a Codex agent that uses `view_image` blows the context
window to 0% after 2-3 turns; images are described vaguely/wrongly.

> NOTE (read this first): the production bug is in **CLIProxyAPI**, not
> Kiro-Go. An earlier draft of this doc concluded "Kiro-Go bug" before the
> real request topology was mapped. Both repos were fixed (same class of bug
> in each), but the one that fixes the live `unlimitedapi.fyi` flow is the
> CLIProxyAPI fix. See "Corrected topology" below.

## TL;DR

- A Codex `view_image` result is delivered as a Responses input item:
  `{"type":"function_call_output","output":[{"type":"input_image","image_url":"data:image/<fmt>;base64,..."}]}`
- Routers flattened that `output` array to a string (`output.String()` in CPA;
  `stringifyArbitrary` in Kiro-Go), turning the image into plain text.
- Effect: the model never receives a real image, and the base64 data-URI is
  counted as text tokens. A 6.7 KB / 512x512 icon cost ~12.8k tokens instead of
  ~350; two ~300 KB screenshots cost ~196k+ (and overflowed the 258,400 window).
- Fix: detect image parts in the tool output and preserve them as a structured
  multimodal content array so the downstream image extraction turns them into
  real vision input. Text-only outputs are unchanged.

## Corrected topology (the live path)

```
codex (originator codex_exec, wire_api = responses)
  -> https://unlimitedapi.fyi            (Caddy: /etc/caddy/Caddyfile)
       handle /v1*  -> 127.0.0.1:8080  (socat) -> 127.0.0.1:8787 (node gateway)
       handle /     -> 127.0.0.1:8317  (CLIProxyAPI)
  -> CLIProxyAPI :8317                    (systemd: cliproxyapi.service)
       /v1/responses  --[translate responses->openai chat]-->  /chat/completions
  -> Kiro-Go :18080                       (systemd: kiro-go.service)
       openai-compatibility provider "kiro-go", key kiro-go-local-test
  -> Kiro / CodeWhisperer backend (AWS)
```

Key consequence: **CLIProxyAPI talks to Kiro-Go over `/chat/completions`**, not
`/v1/responses`. So the responses->chat translation (and the image-flattening
bug) happens *inside CLIProxyAPI*, one hop before Kiro-Go. Kiro-Go's own
`/v1/responses` handler is only exercised if a client hits Kiro-Go's responses
endpoint directly (not the live path here).

9router is NOT in this path. Confirmed via its DB (`~/.9router/db/data.sqlite`)
and Caddyfile — `unlimitedapi.fyi` routes to CLIProxyAPI:8317.

## Root cause (two repos, same bug)

### CLIProxyAPI (production-critical)

File: `internal/translator/openai/openai/responses/openai_openai-responses_request.go`
Function: `ConvertOpenAIResponsesRequestToOpenAIChatCompletions`, case
`function_call_output`:

```go
if output := item.Get("output"); output.Exists() {
    toolMessage, _ = sjson.SetBytes(toolMessage, "content", output.String()) // BUG
}
```

`output` is a JSON array `[{"type":"input_image","image_url":"data:..."}]`.
`gjson`'s `.String()` on an array returns the raw JSON text, so the image
data-URI becomes the literal string content of the tool message. The downstream
`internal/translator/openai/kiro/chat-completions` translator (`contentText`)
*can* extract images, but only from a structured array — it now receives a
string, so the image is lost and shipped as text.

Note: the same file's regular `message` case (line ~153) already handles
`input_image` correctly. Only the `function_call_output` case was broken.

### Kiro-Go (direct /v1/responses path only)

File: `proxy/responses_input.go`, `convertResponsesInputItems`, case
`function_call_output` / `tool_result`:

```go
out := stringifyArbitrary(obj["output"])   // BUG: flattens image array to string
messages = append(messages, OpenAIMessage{Role: "tool", Content: out, ToolCallID: callID})
```

Downstream `proxy/translator.go` `case "tool":` is image-aware
(`extractOpenAIUserContent` -> `extractImageFromOpenAIPart` -> `KiroImage`),
but only when `Content` is a structured array. The stringify destroyed the
structure first.

## Fixes

### CLIProxyAPI
Added `toolOutputContentParts(output gjson.Result) ([]byte, bool)`. When the
tool output is an array containing image parts, it builds a chat-completions
content array (`[{"type":"image_url","image_url":{"url":"..."}}, {"type":"text",...}]`)
so the openai->kiro translator extracts real vision input. Returns
`(nil, false)` when there are no image parts → unchanged plain-string path.

Branch: `fix/responses-tool-image-tokens` on `dat-lequoc/CLIProxyAPI` (private).
Commit: `5a7a1b2f`.
Tests: `..._request_test.go` — `TestConvertOpenAIResponsesRequest_ToolOutputImagePreserved`,
`...ToolOutputTextStaysString`.

### Kiro-Go
Added `toolOutputParts(raw interface{}) []interface{}` in
`proxy/responses_input.go`. Same idea: preserve structured parts when an image
is present so the existing `case "tool":` vision path runs; nil otherwise.

Branch: `fix/responses-tool-image-tokens` on `dat-lequoc/Kiro-Go-private`.
Commit: `ed44056`.
Tests: `proxy/responses_tool_image_test.go` —
`TestResponsesToolOutputImagePreserved`, `TestResponsesToolOutputTextStillString`.

## Deployment (how it was shipped on this box)

Both services run under user/system systemd, executing the repo `bin/` binaries:
- `cliproxyapi.service` -> `/home/nightfury/experiment/CLIProxyAPI/bin/CLIProxyAPI -config config.yaml -local-model`
- `kiro-go.service` -> `/home/nightfury/experiment/Kiro-Go/bin/kiro-go`

Steps performed:
```
# build
cd /home/nightfury/experiment/CLIProxyAPI && go build -o bin/CLIProxyAPI.new ./cmd/server && mv bin/CLIProxyAPI.new bin/CLIProxyAPI
cd /home/nightfury/experiment/Kiro-Go     && go build -o bin/kiro-go .
# restart
systemctl --user restart cliproxyapi.service kiro-go.service   # (kiro-go via the service manager that owns it)
```
Verify live pids + health:
```
pgrep -af "bin/CLIProxyAPI"; pgrep -af "bin/kiro-go"
curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:8317/v1/models -H "Authorization: Bearer <tgk_...>"
curl -s -o /dev/null -w "%{http_code}" http://127.0.0.1:18080/v1/models -H "Authorization: Bearer kiro-go-local-test"
```
Gotcha: a manually started `./bin/kiro-go` had been holding port 18080, which
kept `kiro-go.service` stuck in `activating (auto-restart)`. Kill stray manual
instances before relying on the service.

Confirm the deployed CPA binary contains the fix:
```
strings /home/nightfury/experiment/CLIProxyAPI/bin/CLIProxyAPI | grep -c toolOutputContentParts   # > 0
```

## Verification

### A) Isolated 512x512 icon (Kiro-Go patched, before vs after)

| path | turn1 (no image) | turn2 (with image) | image delta |
|------|------------------|--------------------|-------------|
| before fix (unpatched) | 19,438 | 32,286 | **12,848** |
| after Kiro-Go fix      | 16,837 | 17,351 | **514** |

Description also changed from a wrong "dark blue boxy container" (reading
mangled text) to an accurate "black ghost icon, white eyes, wavy bottom"
(actual vision). Image integrity proven: sha256 of the base64 in the captured
request == sha256 of source `web/icon.png`.

### B) End-to-end live path, multi-turn + tools (the original failure case)

Through the real chain (codex -> unlimitedapi.fyi -> CPA -> Kiro-Go), agent in
`~/ebook` ran 5 turns: `ls` -> view attachment-1.jpg (281 KB) -> `cat ticket.json`
-> view attachment-2.jpg (309 KB) -> final reply. It described both screenshots
accurately, kept running through interleaved shell tools, and reached
`STILL-ALIVE-AFTER-IMAGES`.

Per-turn live window occupancy (`model_context_window` = 258,400):

| turn | action | total_tokens |
|------|--------|--------------|
| 1 | ls | 31,817 |
| 2 | view image-1 (281 KB) | 32,078 |
| 3 | cat ticket.json | 33,982 |
| 4 | view image-2 (309 KB) | 34,290 |
| 5 | final reply | 35,947 |

Peak ~35.9k (~86% context still free). Before the fix, those two images as text
were ~196k tokens (≈390k with doubling) and overflowed the window on a single
turn — matching the reported "context 0% after 2-3 calls".

## How to reproduce / re-verify (for the next agent)

1. Confirm topology: `cat /etc/caddy/Caddyfile`; provider in
   `CLIProxyAPI/config.yaml` under `openai-compatibility: - name: kiro-go`
   (`base-url: http://127.0.0.1:18080/v1`).
2. Run the live test (uses the real `tgk_` key from `~/.codex/auth.json`):
   ```
   cd ~/ebook && codex exec -p opus \
     "Run: ls .codex_ticket_artifacts/<id>/; then view_image attachment-1.jpg and attachment-2.jpg, one per turn, describing each; finally reply STILL-ALIVE-AFTER-IMAGES"
   ```
3. Read per-turn usage from the codex rollout:
   `~/.codex/sessions/<date>/rollout-*.jsonl` -> `last_token_usage` /
   `total_token_usage` / `model_context_window`.
4. Optional wire capture: run an aiohttp passthrough proxy and point
   `-c model_providers.9router.base_url` at it (see git history of this work).
   Note codex sends `Authorization: Bearer <key>` from `~/.codex/auth.json`;
   `-c openai_api_key=...` is ignored when auth.json exists. Use a proxy that
   swaps the key, or the `OPENAI_API_KEY` env var, to hit a local instance.

## Related / not fixed here

- `⚠ Model metadata for claude-opus-4.8 not found. Defaulting to fallback
  metadata`: separate issue. Codex lacks this model's metadata, so its
  "Context % left" gauge uses fallback numbers and can mislead. Independent of
  this token bug.

## File index

- CLIProxyAPI fix: `internal/translator/openai/openai/responses/openai_openai-responses_request.go` (`toolOutputContentParts`)
- CLIProxyAPI test: `internal/translator/openai/openai/responses/openai_openai-responses_request_test.go`
- Kiro-Go fix: `proxy/responses_input.go` (`toolOutputParts`)
- Kiro-Go test: `proxy/responses_tool_image_test.go`
