# OmniGo

An AI gateway built in Go. It proxies an OpenAI-compatible API across multiple
providers, routes through user-defined combos, and manages provider credentials
and gateway client keys.

Performance and simplicity are the point.

| Vision        | What it means                                          |
| ------------- | ------------------------------------------------------ |
| **Fast**        | In-memory routing, no database, SSE passthrough        |
| **Simple**      | One binary; stdlib plus `yaml.v3` and the OpenTelemetry SDK |
| **Pluggable**   | Providers implement a unified `Provider` interface     |
| **Configurable**| Everything lives in two YAML files, hot-reloaded       |

## Features

- **Multi-provider**
  - `openai` — any OpenAI-compatible endpoint (OpenAI, OpenRouter, Groq,
    Ollama, …) with a Bearer API key.
  - `antigravity` — Google *Gemini Code Assist* via OAuth2 (auth-code flow,
    automatic token refresh, OpenAI↔Gemini translation).
  - `codex` — ChatGPT Codex via OAuth2 with PKCE and automatic rotating-token
    refresh, translating the Responses SSE API to OpenAI chat completions.
  - Models are fetched from each provider's live or official catalog and cached;
    Codex retains a conservative static fallback when its catalog is unavailable.
- **API key management** — issue client keys (`ak-…`) to access the gateway.
- **Combo management** — route one model name across a fallback chain with
  `priority` or `fill-first` strategy.
- **Model connection test** — ping any provider and report status/latency.
- **HTMX dashboard** — manage providers, combos, keys, and OAuth logins.
- **Observability** — optional OpenTelemetry metrics (HTTP duration,
  time-to-first-byte, in-flight requests, provider/combo outcomes) exposed in
  Prometheus format at `/actuator/metrics`. Disabled by default.

## Quick Start

### Binary

```bash
# Build and run directly (auto-initializes ~/.config/omnigo/config.yaml on first launch)
go build -o omnigo .
./omnigo

# Or run with live reload during dev
air
```

The version shown in the dashboard header, `-version`, and `/health` comes from
the Git tag stamped into the binary at build time. Release binaries and images
are stamped automatically; a plain `go build` reports `dev`, and
`go install github.com/ac-kurniawan/omnigo@v0.8.5` reports the installed module
version.

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

Dashboard: `http://localhost:8080` (HTTP Basic Auth: `admin`/`admin` by default, or set via `OMNIGO_DASH_USER` / `OMNIGO_DASH_PASS`; toggle with `dashboard.auth` in `config.yaml`; serve with `dashboard.enabled`, default `true`) · API: `http://localhost:8080/v1`

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

### Timeouts

Two knobs bound every upstream call, and both are hot-reloaded:

```yaml
server:
  timeout: 20s          # default 20s
  stream_timeout: 10m   # default 10m; 0 = no total bound
```

- `timeout` bounds how long the gateway waits on a provider. A buffered call is
  capped end to end; a streamed generation is capped per silence, so a long
  answer is not killed mid-flight. Server keep-alive and shutdown bounds are
  fixed transport constants, independent of this value.
- `stream_timeout` caps the total wall-clock time of one streamed generation,
  time to first byte included. Silence alone cannot end a stream that keeps
  trickling bytes; this budget can. `0` (or `0s`) disables it, leaving a
  generation bounded only by silence.

Both take a per-provider override (`providers[].timeout`,
`providers[].stream_timeout`), which wins over the server value. Nothing caps
time to first byte separately: the configured value governs it for streams and
buffered calls alike.

### Dashboard (optional)

The dashboard is served by default. Set `dashboard.enabled: false` in
`config.yaml` to turn it off: the UI, its static assets, and the `/internal/*`
helpers then answer `404`, and basic auth is not applied. The toggle is
hot-reloaded, and `/v1/*`, `/health`, and `/actuator/metrics` are unaffected.
Use it when the management UI should not be reachable where the gateway runs.

```yaml
dashboard:
  enabled: false
```

### Quota (optional)

OmniGo polls each OAuth provider's quota endpoint in the background and shows
per-account quota in the dashboard, on the provider's account cards.

```yaml
quota:
  enabled: true      # default: true
  interval: 5m       # default: 5m, minimum 1m
```

- **`enabled`** turns background polling on or off. While off, no quota
  requests are made and the dashboard shows no quota badges.
- **`interval`** is the polling period. It has a minimum of `1m`: polling
  faster than that would add the upstream pressure the quota data exists to
  avoid. Each cycle is jittered so pooled accounts do not poll in lockstep, and
  an account that keeps failing is retried with backoff.

Polling is display-only. An exhausted reading updates the dashboard badge and
never removes an account from rotation: the next request tries every stored
account, and a failure fails over to the next account only inside that
request.

Two rules keep a bad quota read from looking like exhaustion:

1. A failed or unparseable quota read is reported as **unavailable**.
2. Codex exhaustion follows `rate_limit.allowed`/`limit_reached`, **not**
   `used_percent`. An account can serve normally with a window at 100%.

Quota data is held in memory only and never written to disk. Quota endpoints
for both providers are private upstream interfaces and may change without
notice; when they do, the affected account reports **unavailable**.

### Metrics (optional)

Set `observability.metrics: true` in `config.yaml` to expose Prometheus-format
metrics at `/actuator/metrics` (no authentication, like `/health`). The toggle
is hot-reloaded: enabling or disabling it takes effect without a restart, and
while disabled the endpoint answers `404`.

```bash
curl http://localhost:8080/actuator/metrics
```

Recorded series:

| Metric | Type | Labels |
| ------ | ---- | ------ |
| `http_server_request_duration_seconds` | histogram | `http_route`, `http_request_method`, `http_response_status_code`, `api_key_id` |
| `http_server_request_time_to_first_byte_seconds` | histogram | `http_route`, `http_request_method`, `api_key_id` |
| `http_server_active_requests` | up/down counter | `http_request_method` |
| `omnigo_provider_requests_total` | counter | `gen_ai_system`, `gen_ai_request_model`, `result`, `api_key_id` |
| `omnigo_combo_attempts_total` | counter | `omnigo_combo_name`, `result`, `api_key_id` |
| `omnigo_config_reloads_total` | counter | `result` |
| `omnigo_provider_quota_remaining_ratio` | gauge | `gen_ai_system`, `account`, `window` |
| `omnigo_provider_quota_status` | gauge | `gen_ai_system`, `account` |
| `omnigo_provider_quota_resets_in_seconds` | gauge | `gen_ai_system`, `account`, `window` |

Every label is bounded, because a client able to mint one label value per
request can grow series without limit:

- `http_route` comes from the mux pattern; unmatched requests collapse to
  `unmatched`, so a request path is never a label.
- `http_request_method` is limited to the standard methods; anything else
  becomes `other`.
- `gen_ai_request_model` only takes values from the provider's configured
  catalog; the client-supplied `<provider>/<model>` form is otherwise `other`.
- `result` is a fixed set (`success`, `failure`, `client_abort`,
  `upstream_stall`, `backpressure`, `rate_limited`, `stream_failed`,
  `upstream_error`, `unavailable`).
- `api_key_id` is the id of the gateway client key that authenticated the
  request, as listed in the dashboard's **API Keys** panel: group by it to
  attribute traffic, errors, and latency to a key. It is the short public
  identifier, never the key material or its hash. Requests with no validated
  key (dashboard, health, scrapes, rejected 401s) report `none`, and an id
  outside the identifier alphabet reports `other`; a client-supplied key never
  becomes a label. `http_server_active_requests` has no `api_key_id`: it is
  incremented before authentication runs.
- `account` is a provider credential identity, not a client-supplied value, so
  its cardinality is bounded by the number of configured accounts. Characters
  outside the Prometheus identifier alphabet are replaced (`@` becomes `_at_`),
  and an identity longer than 64 characters is truncated with a short digest
  suffix so distinct accounts never collapse into one series.
- `window` is a quota window name: `primary`/`secondary` for Codex, or a model
  id for Antigravity. A sample with no window records only the status gauge.

#### Quota metrics

The three `omnigo_provider_quota_*` gauges report per-account quota for the
Codex and Antigravity OAuth providers. They are recorded only when background
polling is active (see [Quota](#quota-optional) below).

- `remaining_ratio` is the fraction of the window still available (`1` is
  full, `0` is empty).
- `status` is `1` available, `0` exhausted, `-1` unavailable. `unavailable`
  means the quota read failed or could not be parsed; it is display-only and
  never drains an account.
- `resets_in_seconds` is the countdown to the window reset.

For Codex, a window at `used_percent: 100` is **not** exhaustion: upstream
reports `rate_limit.allowed`/`limit_reached` separately, and an account can
keep serving at 100% of a window. `status` follows those explicit signals.

A reference template is available at [`config.example.yaml`](./config.example.yaml).

### Codex OAuth setup

1. Add a provider with `type: codex` in `config.yaml` or choose **codex** in the dashboard's provider form. No API key is required.
2. Open the provider's **Connect ChatGPT** dialog and click **Open ChatGPT Sign-In**. Complete authorization in the same browser.
3. The browser redirects to `http://localhost:1455/auth/callback`. If nothing is listening there, copy the complete URL from the address bar and paste it into the dashboard dialog. OmniGo exchanges the code and stores the credentials encrypted; do not edit `auth.yaml` manually.
4. Click **Refresh** to cache the current official Codex model catalog, then **Test**. Use a direct model such as `codex-main/gpt-6-astra` or add it to a combo.

The loopback redirect targets the machine running the browser, so a dashboard on a remote server still uses the paste workflow. Run the browser locally and paste the callback into the remote dashboard over a trusted HTTPS connection. Browser-based device authorization is not supported. Datacenter IPs may also be rejected by OpenAI during token exchange; if that occurs, run OmniGo on a network OpenAI accepts. If a rotating refresh token is expired, revoked, reused, or otherwise rejected permanently, OmniGo stops retrying it and reports that re-authentication is required. Use **Connect ChatGPT** again to replace the credentials and resume requests; repeated use of an old refresh token cannot recover the session.

Codex inference and model catalog endpoints are private upstream interfaces and may change without notice. OmniGo sanitizes upstream errors and keeps its bundled model fallback when catalog discovery fails or returns malformed data. Codex quota responses can include five-hour and seven-day reset windows; combo strategies mark only the exhausted provider/model target as drained until a bounded cooldown expires, then automatically retry it.

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
