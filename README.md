# OmniGo

An AI gateway built in Go. It proxies an OpenAI-compatible API across multiple
providers, routes through user-defined combos, and manages provider credentials
and gateway client keys.

Inspired by [OmniRoute](./OmniRoute/README.md) — but OmniRoute is too much.
OmniGo is the deliberately smaller version:

| Vision        | What it means                                          |
| ------------- | ------------------------------------------------------ |
| **Lightweight** | One binary, stdlib only (`yaml.v3` is the sole dep)   |
| **Fast**        | In-memory routing, no database, SSE passthrough        |
| **Modular**     | Providers are a pluggable `Provider` interface         |
| **Configurable**| Everything lives in two YAML files, hot-reloaded       |

## Features

- **Multi-provider, modular**
  - `openai` — any OpenAI-compatible endpoint (OpenAI, OpenRouter, Groq,
    Ollama, …) with a Bearer API key.
  - `antigravity` — Google *Gemini Code Assist* via OAuth2 (auth-code flow,
    automatic token refresh, OpenAI↔Gemini translation).
  - No static model list: models are fetched from each provider's live
    `/models` (or `:fetchAvailableModels`) endpoint and cached.
- **API key management** — issue client keys (`ak-…`) to access the gateway.
- **Combo management** — route one model name across a fallback chain with
  `priority` or `fill-first` strategy.
- **Model connection test** — ping any provider and report status/latency.
- **HTMX dashboard** — manage providers, combos, keys, and OAuth logins.

## Quick Start

### Binary

```bash
# Build and run directly (auto-initializes ~/.config/omnigo/config.yaml on first launch)
go build -o omnigo .
./omnigo

# Or run with live reload during dev
air
```

### Docker & Docker Compose

```bash
# Using Docker Compose
docker compose up -d

# Or using Docker directly
docker run -d \
  --name omnigo \
  -p 8080:8080 \
  -v ~/.config/omnigo:/home/omnigo/.config/omnigo \
  ghcr.io/ac-kurniawan/omnigo:latest
```

Dashboard: `http://localhost:8080` · API: `http://localhost:8080/v1`

```bash
curl http://localhost:8080/v1/chat/completions \
  -H "Content-Type: application/json" \
  -H "Authorization: Bearer ak-…" \
  -d '{"model":"auto","messages":[{"role":"user","content":"Hello!"}]}'
```

## Configuration & Storage

OmniGo stores its configuration files under **`~/.config/omnigo/`** (or `%APPDATA%\omnigo` on Windows):

| File | Location | Description |
| ---- | -------- | ----------- |
| `config.yaml` | `~/.config/omnigo/config.yaml` | Plaintext. Providers, combos, and cached model list. Auto-seeded if missing. |
| `auth.yaml` | `~/.config/omnigo/auth.yaml` | Encrypted at rest (AES-256-GCM). Provider credentials and gateway client keys. |
| `.secret.key` | `~/.config/omnigo/.secret.key` | 32-byte encryption key (auto-generated if unset). |

You can override the directory or individual files via CLI flags or environment variables:
- `-dir <path>` or `OMNIGO_CONFIG_DIR=<path>`
- `-config <path>`, `-auth <path>`, `-key <path>`

A reference template is available at [`config.example.yaml`](./config.example.yaml).

## How it works

```
Client → /v1/chat/completions
  → auth middleware (Bearer ak-… vs stored hash)
  → model resolves to a combo name or a <provider>/<model>
  → combo engine tries targets in order until one succeeds
  → provider forwards / translates the request, streams SSE back
```

- `model: auto` (a combo) → routes across its target chain.
- `model: openai-main/gpt-4o` (direct) → dispatches straight to that provider.

## Architecture

See [`AGENTS.md`](./AGENTS.md) for the project map, conventions, and rules.
The full design and the TDD implementation plan live under `docs/superpowers/`.

## License

MIT
