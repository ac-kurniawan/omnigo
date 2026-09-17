# OmniGo — Agent Guide

> **Single source of truth** for every AI assistant working this repository
> (Claude Code, Codex, Cursor, OpenCode, etc.). When a rule needs to change,
> change it here.

OmniGo is an AI gateway built in Go. It proxies an OpenAI-compatible API across
multiple providers, routes through user-defined combos, and manages provider
credentials and gateway client keys. It is inspired by
[OmniRoute](./OmniRoute/README.md) but deliberately smaller: lightweight, fast,
configurable.

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
  (`go.opentelemetry.io/otel` + `exporters/prometheus`) are the third-party
  dependencies. Everything else is stdlib (`net/http`, `crypto/aes`,
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
| API        | `internal/api`    | `/v1/chat/completions`, `/v1/models`, internal endpoints   |
| Dashboard  | `internal/dashboard` | HTMX UI: providers, combos, keys, OAuth login              |
| Observability | `internal/observability` | OpenTelemetry meter, Prometheus exposition at `/actuator/metrics` |

## Request Pipeline

```
Client → /v1/chat/completions
  → auth middleware (Bearer ak-... vs stored hash)
  → parse OpenAI body
  → model resolves to a combo name or a `<provider>/<model>`
      combo: combo engine orders targets (priority | fill-first)
             → for each target in order until success
      direct: dispatch to that provider
  → provider.ChatCompletion(ctx, req, w)
      openai:      forward request to {base_url}/chat/completions (SSE passthrough)
      antigravity: refresh token if expiring → translate OpenAI→Gemini envelope
                   → POST {base_url}/v1internal:streamGenerateContent?alt=sse
                   → translate Gemini SSE → OpenAI SSE
```

## Config & Secrets

- **`~/.config/omnigo/`** — default configuration root on Linux/macOS (`%APPDATA%\omnigo` on Windows). Auto-seeded on first launch.
- **`config.yaml`** — plaintext, safe to commit or edit (gitignored in root; reference template at `config.example.yaml`). Server, providers (name/type/base_url/models), combos. Models listed here are the *cache* of each provider's live `/models` (or `:fetchAvailableModels`) response.
- **`auth.yaml`** — **encrypted at rest** (AES-256-GCM). Holds provider API
  keys and OAuth tokens (`access_token`, `refresh_token`, `expires_at`,
  `project_id`) plus gateway client-key hashes. Never commit plaintext secrets.
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
3. Never add an unsanctioned third-party dependency when stdlib suffices (only `yaml.v3` and `go.opentelemetry.io/otel` are approved).
4. Never write production code before a failing test exists.
5. Never log or return raw credentials.
6. Keep database-free: all state is in-memory + YAML files. No SQL, no ORM.
7. All state mutations go through the config/vault packages — never write
   `auth.yaml` from a handler.

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

## Reference Docs

- Design spec: `docs/superpowers/specs/2026-09-10-aigo-gateway-design.md`
- Implementation plan (TDD): `docs/superpowers/plans/2026-09-10-aigo-mvp.md`
