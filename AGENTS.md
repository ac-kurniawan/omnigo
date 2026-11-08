# OmniGo — Agent Guide

> **Single source of truth** for every AI assistant working this repository
> (Claude Code, Codex, Cursor, OpenCode, etc.). When a rule needs to change,
> change it here.

OmniGo is an AI gateway built in Go. It proxies an OpenAI-compatible API across
multiple providers, routes through user-defined combos, and manages provider
credentials and gateway client keys. Performance and simplicity are the point:
lightweight, fast, configurable.

## Quick Start

```bash
air                           # run with live reload during dev
go build -o omnigo .          # build the single binary
./omnigo -config config.yaml  # run (dashboard on http://localhost:8080)
go test ./...                 # run the full test suite
go vet ./...                  # vet
gofmt -w .                    # format (or goimports)
```

- **Go version**: `1.26+` (`go.mod` pins the floor).
- **Module**: `github.com/ac-kurniawan/omnigo`.
- **Dependencies**: `gopkg.in/yaml.v3` and the OpenTelemetry Go SDK
  (`go.opentelemetry.io/otel`, `exporters/prometheus`, `exporters/otlp/otlptrace/otlptracehttp`)
  are the third-party dependencies. Treat the whole otel family as one unit:
  bump `otel`, `sdk`, `metric`, and the exporters together. Everything else is stdlib (`net/http`, `crypto/aes`,
  `html/template`, `hash`). Do not add a dependency for something a few lines
  of stdlib covers.

## Project at a Glance

| Layer      | Location          | Purpose                                                    |
| ---------- | ----------------- | ---------------------------------------------------------- |
| Entrypoint | `main.go`         | Flags, wiring, config reload loop, server bootstrap        |
| Config     | `internal/config` | Load/validate `config.yaml`, atomic in-memory snapshot     |
| Secrets    | `internal/vault`  | AES-256-GCM encrypt/decrypt of `auth.yaml`                 |
| Gateway auth | `internal/auth` | Client API-key generation, hashing, validation middleware  |
| Providers  | `internal/provider` | `Provider` interface, registry, `openai` + `antigravity` impls |
| Combos     | `internal/combo`  | Routing engine: `priority`, `fill-first`                   |
| System One | `internal/systemone` | TypeSafe System One protocol client: typed questions, model selection |
| API        | `internal/api`    | `/v1/chat/completions`, `/v1/models`, internal endpoints   |
| Dashboard  | `internal/dashboard` | HTMX UI: providers, combos, keys, OAuth login              |
| Observability | `internal/observability` | OpenTelemetry meter, Prometheus exposition at `/actuator/metrics` |
| Tracing | `internal/observability/tracing` | OTLP exporter to Langfuse: one generation span per upstream dispatch, plus the inbound request span |
| Quota      | `internal/quota`  | Per-account quota snapshots, in-memory cache, background poller, opt-in auto-drain |

## Request Pipeline

```
Client → /v1/chat/completions
  → metrics.Middleware (installs the *auth.Caller holder)
  → tracing.Middleware (inbound span; caller id filled after dispatch)
  → auth middleware (Bearer ak-... vs stored hash)
  → parse OpenAI body
  → model resolves to a combo name or a `<provider>/<model>`
      combo: combo engine orders targets (priority | fill-first)
             → for each target in order until success
      smart combo: ask a System One model which route fits the request
                   → the route's destination combo owns the failover
                   → on any decision failure, use the default route
      direct: dispatch to that provider
  → per dispatch: trackAttempt installs one usage sink (token metrics + generation capture)
                  → provider.ChatCompletion(ctx, req, w)
                    w is wrapped by observingWriter only when capture_content is on
      openai:      forward request to {base_url}/chat/completions (SSE passthrough)
      antigravity: refresh token if expiring → translate OpenAI→Gemini envelope
                   → POST {base_url}/v1internal:streamGenerateContent?alt=sse
                   → translate Gemini SSE → OpenAI SSE
  → RecordProviderRequest + RecordGeneration (one span per attempt that dispatched)
```

## Config & Secrets

- **`~/.config/omnigo/`** — default configuration root on Linux/macOS (`%APPDATA%\omnigo` on Windows). Auto-seeded on first launch.
- **`config.yaml`** — plaintext, safe to commit or edit (gitignored in root; reference template at `config.example.yaml`). Server, providers (name/type/base_url/models), combos. Models listed here are the *cache* of each provider's live `/models` (or `:fetchAvailableModels`) response.
- **`auth.yaml`** — **encrypted at rest** (AES-256-GCM). Holds provider API
  keys and OAuth tokens (`access_token`, `refresh_token`, `expires_at`,
  `project_id`), gateway client-key hashes, and the Langfuse project key pair
  (`langfuse.public_key` / `langfuse.secret_key`) saved from the Settings tab.
  Never commit plaintext secrets.
- **`.secret.key`** — 32-byte key material, auto-generated on first run if
  `OMNIGO_SECRET_KEY` (or `AIGO_SECRET_KEY`) is unset. **gitignored.** Losing it means re-entering
  credentials.
- **Reload**: the config watcher re-reads `config.yaml` (and `auth.yaml`) on
  mtime change and swaps the in-memory snapshot atomically. No restart needed.

## Code Conventions

- **Go style**: `gofmt`, idiomatic stdlib. Errors wrapped with `%w`.
  Table-driven tests. Keep handlers thin; put logic in packages.
- **File layout**: one clear responsibility per file. Small focused files over
  large ones. `internal/` packages map 1:1 to the table above.
- **Naming**: packages lowercase single-word (`config`, `vault`, `auth`,
  `provider`, `combo`, `api`, `dashboard`); exported identifiers as needed.
- **Concurrency**: the config snapshot is behind `atomic.Value` or
  `sync.RWMutex`. Providers are safe for concurrent use (no shared mutable
  request state). Never mutate shared state across concurrent requests.

## Security

- **Never** log tokens, API keys, or the `auth.yaml` plaintext.
- **Never** commit `auth.yaml` or `.secret.key` (both gitignored).
- OAuth client credentials for Antigravity (`client_id`/`client_secret`) are
  **public values shipped in the Antigravity CLI** — they are defaults, not
  secrets, and may be overridden in config. Only the *tokens* are secrets.
- **Encrypt at rest**: all `auth.yaml` values go through `internal/vault`
  (AES-256-GCM). No plaintext fallback.
- Validate all request bodies before forwarding. Sanitize error responses —
  never return raw upstream `err` strings that could leak tokens.

## Testing (TDD)

Follow [TDD](docs/superpowers/plans/2026-09-10-aigo-mvp.md) — the Iron Law:
**no production code without a failing test first.**

- Unit tests live next to the package under test (`internal/<pkg>/*_test.go`),
  standard `go test`.
- Prefer real code over mocks. For HTTP, use `net/http/httptest` servers as
  fake upstreams instead of a mocking library.
- Every bug fix starts with a failing test that reproduces it.

```bash
go test ./internal/combo/ -run TestPriority -v   # focused
go test ./...                                    # full suite
```

## Hard Rules

1. Never commit secrets, tokens, or API keys.
2. Never commit `auth.yaml` or `.secret.key`.
3. Never add an unsanctioned third-party dependency when stdlib suffices (only `yaml.v3` and the `go.opentelemetry.io/otel` family, including the Prometheus and OTLP trace exporters, are approved).
4. Never write production code before a failing test exists.
5. Never log or return raw credentials.
6. Keep database-free: all state is in-memory + YAML files. No SQL, no ORM.
7. All state mutations go through the config/vault packages — never write
   `auth.yaml` from a handler.
8. **No False Assumptions / Honest Verification Before Claiming Done**:
   - Never assume an issue is merely caused by user deployment/environment before verifying the complete, real data-flow pipeline (input -> storage/vault -> cache -> rendering).
   - Mock unit tests with synthetic happy-path fields (e.g. artificial email/account IDs) DO NOT prove real-world behavior: always test with the exact minimal payload shape produced by actual user inputs.
   - Never claim a UI feature or visual element is working without inspecting the real rendered DOM/HTML or screenshot produced from the actual data state.

## Common Modification Scenarios

### Adding a provider type

1. Implement `provider.Provider` in `internal/provider/<name>.go`.
2. Register it in `internal/provider/registry.go`.
3. If it needs OAuth, add the flow to `internal/provider/<name>/` (see the
   antigravity package) and wire the login/callback routes in
   `internal/dashboard`.
4. Write tests with an `httptest` upstream before the implementation (TDD).

### Adding an endpoint

1. Add the route in `internal/api/router.go` (Go 1.22+ `ServeMux` patterns).
2. Handler delegates to package logic — keep the handler thin.
3. Auth middleware applied for everything under `/v1/`.
4. Test the handler with `httptest.NewRequest` + a fake provider.

### Adding a combo strategy

1. Implement the ordering/fill logic in `internal/combo/`.
2. Register the strategy string in the combo config validation.
3. Test that a failing first target falls back to the second (TDD).

The `smart` strategy is different: it decides *which* chain to run rather than
ordering one. Its decision lives in `internal/api/smart.go` (the router), the
protocol client in `internal/systemone/`, and the config in
`internal/config/smart.go`. A smart combo has no targets of its own, so
`PruneComboTargets` must leave it alone.

`internal/systemone` speaks the System One protocol, not one model. Jev is
TypeSafe's flagship System One model and the first one, but every System One
model is served by the same `POST /v1/systemone` endpoint and the request's
`model` field picks the handler. So the model is configuration, and the
package must not hard-code Jev's name, id, or behaviour. A smart combo's
`decision.model` names any model the account can use; `GET /system-one/models`
lists them.

### Adding a traced dispatch

Every upstream call site must produce exactly one generation. The pattern is
fixed, and two rules in it are load-bearing:

```go
cap, providerCtx, providerWriter := trackAttempt(ctx, metrics, t.Provider, t.Model, cb.Name, attempt, capture)
providerErr := p.ChatCompletion(providerCtx, req, providerWriter)
err := finishAttempt(attempt, providerErr)   // always the ORIGINAL attempt writer
result := classifyProviderError(err, errors.Is(err, errResponseCommitted), r.Context().Err() != nil)
metrics.RecordProviderRequest(ctx, t.Provider, t.Model, result)
metrics.RecordGeneration(ctx, generationFrom(start, time.Now(), "chat", t.Provider, t.Model, cb.Name, cap, result, req, capture))
```

1. **One usage sink per attempt.** `trackAttempt` replaces `withTokenUsage`.
   Calling `provider.WithUsageSink` a second time overwrites the first, so
   token metrics or the generation record would silently go missing. Extend
   the single callback instead of stacking another.
2. **The observer never replaces the attempt writer.** `finishAttempt` decides
   failover from the attempt's commit state, so it keeps the original writer;
   `observingWriter` only sits between the provider and that writer, and
   forwards `Flush` unconditionally (SSE depends on it).

Record a generation only after the provider was actually called: an unknown
provider or a disabled model returns before dispatch and must produce no span.
`capture` comes from `CaptureGeneration(getCfg())`, computed once per request;
content is only read when it is true.

## Reference Docs

- Design spec: `docs/superpowers/specs/2026-09-10-aigo-gateway-design.md`
- Implementation plan (TDD): `docs/superpowers/plans/2026-09-10-aigo-mvp.md`
