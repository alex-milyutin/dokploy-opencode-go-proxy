# dokploy-opencode-go-proxy

An OpenAI-compatible passthrough that lets Dokploy's AI panel use an **OpenCode Go**
subscription instead of buying separate OpenCode Models credits.

Russian version: [README.RU.md](README.RU.md)

## Why this exists

Dokploy builds its provider through `@ai-sdk/openai-compatible` and hardcodes the
`Authorization: Bearer …` header only
(`packages/server/src/utils/ai/select-ai-provider.ts`, `custom` branch). There are no
user-supplied headers in `Settings → AI`: the fields are just
`Name / API URL / API Key / Model` (`apps/dokploy/server/api/routers/ai.ts`).

OpenCode Go requires a stable `x-opencode-session` header on every non-GET request (see
"Where can I use it?" in the Go docs) and otherwise answers with `400 MissingSessionID`:

```
Request is missing x-opencode-session and cannot be routed efficiently.
```

This proxy adds that header (plus its own `User-Agent`) and forwards everything else
untouched.

```
Dokploy AI panel ──► http://opencode-go-proxy:8080/v1 ──► https://opencode.ai/inference/go/openai/v1
  Authorization: Bearer <PROXY_TOKEN>     + x-opencode-session: ses_...
                                          + User-Agent: dokploy-ai/1.0
                                          + Authorization: Bearer <OPENCODE_API_KEY>
```

## What it proxies

| Method | Path | Behavior |
|---|---|---|
| `GET` | `/models` | forwarded to `{UPSTREAM_BASE}/models` (no session needed) |
| `POST` | `/chat/completions` | body and status returned as-is, `x-opencode-session` added; `stream: true` is passed through chunk by chunk |
| `GET` | `/healthz` | `ok`, used by the container healthcheck |

Upstream responses are copied verbatim (status, body, headers), so messages such as
"Insufficient balance" reach Dokploy unchanged. The response carries an extra
`X-Proxy-Session` header showing which session the request used.

## Environment variables

| Variable | Required | Default | Meaning |
|---|---|---|---|
| `OPENCODE_API_KEY` | yes | — | the OpenCode Go API key; replaces whatever key the client sent |
| `PROXY_TOKEN` | strongly recommended | — | the value typed into Dokploy's **API Key** field; without it the proxy is open to anyone |
| `UPSTREAM_BASE` | no | `https://opencode.ai/inference/go/openai/v1` | upstream base URL |
| `LISTEN_ADDR` | no | `:8080` | listen address |
| `SESSION_MODE` | no | `prefix` | `prefix`, `constant` or `request` (see below) |
| `SESSION_ID` | no | `ses_dokploy_proxy` | value used by `constant` and as a fallback |
| `USER_AGENT` | no | `dokploy-ai/1.0` | own user agent, as the Go docs ask for |

### About the session header

Go wants a session that is stable per conversation, but Dokploy has no conversations:
every call is a single `generateText` with one `prompt` and no history. The "conversation"
is therefore approximated from the instruction part of the prompt:

- `prefix` (default) — `ses_` + `sha256(model + first 256 characters of the first message)`.
  Each Dokploy feature has its own prompt template (build log analysis, runtime log
  analysis, compose generation, variant suggestions) while the volatile part (the logs
  themselves) starts past character 256. The result is stable per feature and different
  between features, which is exactly the prompt-cache affinity Go routes on.
- `constant` — one session per instance. Simplest; all traffic lands on one backend.
- `request` — a new id per request. Not recommended: loses cache affinity.

## Build and run locally

```bash
GOTOOLCHAIN=go1.24.9 go build -o ocproxy .
OPENCODE_API_KEY=oc_sk_... PROXY_TOKEN=sk-local UPSTREAM_BASE=https://opencode.ai/inference/go/openai/v1 ./ocproxy

curl -s http://127.0.0.1:8080/healthz
curl -s -H 'Authorization: Bearer sk-local' http://127.0.0.1:8080/v1/models
```

Docker:

```bash
docker build -t dokploy-opencode-go-proxy .
docker run --rm -p 8080:8080 -e OPENCODE_API_KEY=oc_sk_... -e PROXY_TOKEN=sk-local dokploy-opencode-go-proxy
```

Go 1.24 is required: binaries built with older toolchains can fail to start on recent
macOS with `dyld: missing LC_UUID load command`.

## Deploy inside Dokploy

The proxy runs inside Dokploy itself. Two options.

### Option A: Compose service (recommended)

`Create Service → Compose` with this directory as the repository (or paste
`docker-compose.yml` and `Dockerfile` into the editor). Dokploy builds the image from
`build: .`, runs it, and attaches it to the `dokploy-network` network the panel itself
lives on (the installer runs `docker network create --driver overlay --attachable
dokploy-network` and starts the panel with `--network dokploy-network`). No port is
published: the panel reaches the service over that network.

`OPENCODE_API_KEY` and `PROXY_TOKEN` go into the service's `Environment` (Dokploy stores
them encrypted). `docker-compose.yml` uses `${VAR:?set VAR}`, so a missing variable fails
the deploy instead of starting an unauthenticated proxy.

### Option B: Application

`Create Service → Application` with the Dockerfile provider, build path = this
directory's root, port `8080`, no domain needed. Dokploy picks up the healthcheck from
the Dockerfile.

### Which URL to put in `Settings → AI`

Check in this order, cleanest first:

1. `http://opencode-go-proxy:8080/v1` — the compose service name on `dokploy-network`.
   The panel resolves it through Docker DNS. This is what the shipped compose file is
   set up for: it publishes no port at all.
2. `http://<node private IP>:8080/v1` — only if the DNS name does not resolve. Re-add a
   `ports: ["8080:8080"]` block to the compose file, or bind it to a private interface.
   Private addresses are not blocked by self-hosted Dokploy: the private-network list in
   `provider-fetch.ts` only applies under `IS_CLOUD` (cloud metadata addresses are always
   blocked).
3. `https://ai-proxy.<domain>/v1` through Traefik — only if you need access from outside
   the host. Then `PROXY_TOKEN` is mandatory.

⚠️ Publishing `8080:8080` is not private by itself: the port answers on the node's
hostname, and Cloudflare proxies port 8080, so the proxy becomes reachable from the
internet even without a dedicated domain. It stays token-protected, but prefer the
service name or a firewall.

Warning: `http://127.0.0.1:8080/v1` will not work — the panel runs in a container, so
127.0.0.1 is the panel itself.

Remaining fields:

| Field | Value |
|---|---|
| Name | `OpenCode Go (proxy)` |
| API URL | one of the addresses above |
| API Key | the `PROXY_TOKEN` value (not the Go key — that stays on the server) |
| Model | `deepseek-v4.1-flash`, `kimi-k3`, `glm-5.3`, ... (any Go text model) |

The model list is fetched from `/v1/models`, so the dropdown shows the real Go model IDs
after saving. Protocol does not matter: Console translates between all four APIs, so
models whose native endpoint is anthropic/responses also work over `chat/completions`.

### Verify after deploying

```bash
curl -s http://opencode-go-proxy:8080/healthz          # ok
curl -s -H 'Authorization: Bearer <PROXY_TOKEN>' http://opencode-go-proxy:8080/v1/models
```

In Dokploy: `Settings → AI → Test connection`, then run any log analysis. The proxy logs
one line per call:

```
POST /v1/chat/completions -> 200 in 640ms session="ses_78445081cec0a32b0ad919fd"
```

A `400 MissingSessionID` response means one of two things: the request bypasses the proxy
(check the API URL) or the old image is still running.

## What it does not do

- **No protocol translation** — not needed, Console translates on its own side.
- **No conversation history** — Dokploy sends a single prompt, so a "session" is derived
  from the instruction rather than reflecting a real dialogue. Consequence: parallel calls
  of the same feature share one backend and one KV cache. That is fine at the request
  volume of the Dokploy panel; switch to `SESSION_MODE=request` if throttling shows up.
- **Not a replacement for** [`Dokploy#5576`](https://github.com/Dokploy/dokploy/issues/5576)
  / PR #5577 (custom per-provider headers). Once that merges, the proxy can be retired:
  point Dokploy straight at the Go endpoint and set `x-opencode-session` in the UI.

## Tests

```bash
python3 test/mock_upstream.py 9099 &
PROXY_BIN=./ocproxy scripts/smoke_test.sh
```

`test/mock_upstream.py` is a fake OpenAI-compatible upstream that records every request;
`scripts/smoke_test.sh` asserts: 401 without the proxy token, `/v1/models` passthrough,
session stability inside one feature, difference between features, key replacement, own
user agent, and SSE streaming. The real gateway is verified separately: with a
deliberately invalid key it answers `401 authentication_error` rather than
`MissingSessionID`, which confirms the path and headers arrive correctly.
