# Kiro API Keys (`ksk_`) — How They Work and How Kiro-Go Supports Them

This document records the research into Kiro's long-lived API keys (the
`ksk_...` credentials) and explains the support that Kiro-Go ships for them.

## 1. What a `ksk_` key is

`ksk_...` is a **Kiro Secret Key**: a long-lived API key intended for headless
use of `kiro-cli` (CI/CD pipelines, automation scripts). It is a distinct
credential type from the OAuth refresh tokens that Kiro-Go's other account
import methods use.

Sources (Kiro official docs):

- `https://kiro.dev/docs/cli/authentication/` — section "Authenticate with an
  API key (headless mode)".
- `https://kiro.dev/docs/enterprise/governance/api-keys/` — admin toggle to
  allow users to generate keys.

Key facts from the docs:

- Only **Pro / Pro+ / Pro Max / Power** subscribers can mint keys. For
  admin-managed subscriptions, the admin must enable API key generation first.
- The full key value is shown only once, at creation time.
- Credits consumed by the key are decremented from the subscription's credits.
- Sanctioned usage is exactly:

  ```bash
  export KIRO_API_KEY=ksk_xxxxxxxx
  kiro-cli chat --no-interactive "your prompt here"
  ```

## 2. How the authentication actually works (reverse-engineered)

The findings below were established by probing the live AWS endpoints with a
real key, tracing `kiro-cli` with `-vvv`, and tracing its syscalls.

1. The CLI reads `KIRO_API_KEY` and **exchanges** the `ksk_` for a short-lived
   bearer via **AWS SSO-OIDC** at `https://oidc.us-east-1.amazonaws.com`. A
   clean unproxied run connects to an `oidc.us-east-1.amazonaws.com` address
   (confirmed via `strace`), and the binary embeds `aws-sdk-ssooidc` plus an
   auth-scheme enum that lists `ApiKey` as a first-class mode alongside
   `Social` / `BuilderId` / `ExternalIdp`.
2. The exchange returns a bearer valid for **~15 minutes** (the identity cache
   records `valid_for≈899s`). The token lives in memory only — it is never
   persisted to the CLI's `data.sqlite3`.
3. The CLI then calls **CodeWhisperer / Amazon Q** at
   `https://q.us-east-1.amazonaws.com` (`GetProfile`, `ListAvailableModels`,
   `GenerateAssistantResponse`) signed with the short-lived bearer.

Important negative result:

- The raw `ksk_` is **not** a usable bearer on its own. Sending
  `Authorization: Bearer ksk_...` directly to any Q / CodeWhisperer operation
  returns `403 "The bearer token included in the request is invalid."`.

### Why Kiro-Go does not implement the exchange natively

The exact body of the SSO-OIDC `CreateToken` request for the API-key grant
could not be captured. `kiro-cli` is a Rust binary using `rustls` +
`aws-lc-rs` with a compiled-in trust store, so:

- mitmproxy's CA is rejected (TLS pinning), and
- `SSLKEYLOGFILE` is honored only by the bundled `bun` runtime, not by the
  AWS SDK connections.

Direct probes of `oidc.us-east-1.amazonaws.com/token` show the service does
recognize `clientId=ksk_...` (the error changes from `invalid_client` to
`invalid_grant`), but the exact grant parameters are undocumented and were not
recoverable in this environment. A native exchange therefore cannot be
implemented reliably yet.

## 3. Kiro-Go's support: the `kiro-cli` bridge

Because the only proven, stable way to use a `ksk_` key is through `kiro-cli`
itself, Kiro-Go supports these keys via a **subprocess bridge**.

- A new account `authMethod` value: **`apikey`**.
- For such accounts, the `ksk_` key is stored in the account's `apiKey` field
  (see `config.Account.ApiKey`).
- When a request is routed to an `apikey` account, Kiro-Go invokes the local
  `kiro-cli` binary with `KIRO_API_KEY` set to the key, feeds it the flattened
  prompt on stdin, and streams the cleaned stdout back through the normal
  Claude/OpenAI response path.

### Requirements

- The `kiro-cli` binary must be installed and on `PATH` (or its path set via
  the `KIRO_CLI_PATH` environment variable) on the machine running Kiro-Go.
- The key must belong to a Pro/Pro+/Pro Max/Power subscription.

### Limitations (by design, inherent to the bridge)

- **Tool use / function calling is not supported** through the bridge. The
  `kiro-cli --no-interactive` path returns assistant text only; structured
  tool-call turns are flattened to text context.
- **Images are not forwarded.**
- **Token counts are estimated**, not authoritative. Credits are parsed from
  the CLI's stderr `▸ Credits: X` line when present.
- Throughput is bounded by `kiro-cli` process startup (~1-2s per request) and
  the CLI's own ~15-minute internal token caching.
- The model is passed via `--model`; if the CLI rejects it, it falls back to
  its default model.

### Adding an API-key account

Via the admin API:

```bash
curl -X POST http://localhost:8080/admin/api/auth/apikey \
  -H "X-Admin-Password: <admin-password>" \
  -H "Content-Type: application/json" \
  -d '{"apiKey":"ksk_xxxxxxxx","nickname":"my-pro-key"}'
```

This validates the key by running `kiro-cli chat --list-models --format json`
with it before persisting anything. A rejected key yields only the static
`auto` fallback entry (which the bridge skips), so an empty model catalog is
treated as an auth failure and the import is refused. A valid key stores an
`apikey` account that participates in the normal pool / round-robin routing
alongside OAuth accounts. The request body accepts multiple keys (one per
line) for batch import.

Via the admin web panel: open **Add Account → Kiro Secret Key (ksk_)**, paste
one or more keys (one per line), optionally set a nickname, and submit.

### Runtime failure handling

`kiro-cli` sometimes exits with code 0 even when the key is rejected, printing
`Authentication failed. Your API key may be invalid or expired.` to stderr.
The bridge therefore scans stderr for auth/quota markers in addition to the
process exit code, and surfaces a classifiable `http 403` / `429` error so the
account failover layer can cool down or disable the offending key instead of
returning an empty "successful" response.

## 4. Summary

| Aspect | OAuth accounts (`idc`/`social`) | API key accounts (`apikey`) |
|--------|----------------------------------|------------------------------|
| Credential | refresh token (+clientId/secret) | `ksk_` key |
| Upstream call | direct HTTPS to Q/CodeWhisperer | via `kiro-cli` subprocess |
| Token refresh | OIDC/social refresh endpoint | handled internally by `kiro-cli` |
| Tool use | supported | not supported |
| Images | supported | not forwarded |
| Streaming | native event stream | line-buffered from CLI stdout |
