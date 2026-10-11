# lite-model-router — MVP plan

## 1. Why this exists

| Option | Fit for "3 home boxes + 1 cloud key, one URL per role" |
|---|---|
| **LiteLLM proxy** | Does fallback chains, but ships Python + ~100 transitive deps, optional Postgres/Redis, budgets, UI, provider translation. Heavy to install on a mini PC, large attack surface, slow cold start. |
| **nginx / Caddy / HAProxy** | Fast, but cannot read the JSON body to pick a backend by `model` or rewrite `model` per backend; health checks for "first healthy in ordered list" plus per-backend model rename need scripting/plugins. |
| **Ollama / llama-swap** | Swap models on *one* host; do not fail over across hosts/providers. |
| **lite-model-router** | One static Go binary, stdlib only, one JSON file. Reads the body only to resolve/rewrite `model`, then streams bytes through. |

Target user: a handful of self-hosted servers (llama.cpp, vLLM, MLX, Ollama…) that come and go, plus maybe a cloud API, consumed by clients that accept one endpoint + one model name per role (e.g. Qwen Code's vision model).

## 2. Assumptions (explicit)

- A1. Backends are already OpenAI- or Anthropic-compatible. **No format translation.** A virtual model may mix OpenAI and Anthropic backends, but the client's request goes verbatim (except `model`) — it is the operator's job to only chain compatible backends per path.
- A2. The model name is in the top-level JSON field `model`, or (multipart) the form field `model`, or the `X-Model` header. `GET /v1/models` is answered locally; any other request without a model gets 400.
- A3. Request bodies are bounded (default `max_body_bytes` = 64 MiB) and fully buffered in memory once so they can be replayed on retry. Responses are never buffered.
- A4. Single process, in-memory state; restart = reset health (probes re-learn within one interval).
- A5. Trusted operator, possibly untrusted LAN clients (hence optional inbound auth, loopback default bind).

## 3. Design

```
client ──► [auth] ──► [read body ≤ max] ──► resolve model ──► chain = [(backend, upstream_model)...]
                                                   │
                for each candidate in chain, healthy first (see §5):
                    rewrite model → upstream_model, set auth header from env,
                    send; if pre-response failure → mark backend, next candidate
                    else copy status+headers, stream body with flush → done
```

Packages (all in one module `github.com/zhuanyi/lite-model-router`):
- `main.go` — flags (`-config`, `-listen` override, `-quiet`), signal handling, graceful shutdown.
- `internal/config` — load + validate JSON, resolve env secrets at startup.
- `internal/health` — per-backend state, prober goroutines, passive marking.
- `internal/proxy` — HTTP handler: routing, body rewrite, retry, streaming copy, `/v1/models`, `/healthz`, `/status`.

### Endpoints served by the proxy

| Path | Behaviour |
|---|---|
| `GET /healthz` | `200 ok` (liveness of the proxy itself; no auth). |
| `GET /status` | JSON: per-backend `{healthy, last_error, last_change, consecutive_failures, next_probe_in}` (`next_probe_in` omitted for passive-only backends) and virtual model chains. No secrets, no URLs userinfo. Requires inbound auth if enabled. |
| `GET /v1/models` | OpenAI list of virtual models + exposed backend models; generated locally. |
| `* /v1/*` (anything else, incl. `/v1/messages`) | Proxied. |
| other | 404. |

## 4. Config schema (JSON)

```json
{
  "listen": "127.0.0.1:4000",
  "inbound_api_key_env": "LMR_API_KEY",
  "max_body_bytes": 67108864,
  "log_requests": true,
  "max_concurrent_requests": 64,
  "use_env_proxy": false,
  "allow_unauthenticated_remote": false,
  "backends": {
    "gpu-laptop": {
      "base_url": "http://gpu-laptop.local:8080",
      "api_key_env": "",
      "auth_style": "bearer",
      "health": {"path": "/v1/models", "interval": "10s", "timeout": "2s"},
      "connect_timeout": "3s",
      "first_byte_timeout": "300s",
      "expose_models": ["qwen2.5-vl-32b"]
    },
    "cloud": {
      "base_url": "https://api.openai.com",
      "api_key_env": "OPENAI_API_KEY",
      "auth_style": "bearer",
      "health": {"path": "", "interval": "30s"}
    }
  },
  "models": {
    "vision": [
      {"backend": "gpu-laptop", "model": "qwen2.5-vl-32b"},
      {"backend": "mini-pc",    "model": "qwen2.5-vl-7b"},
      {"backend": "cloud",      "model": "gpt-4o-mini"}
    ]
  }
}
```

Rules:
- `base_url` is scheme+host[+port][+path prefix]; the client path (`/v1/...`) is appended. Backends come **only** from config (no SSRF surface).
- `api_key_env`: name of env var; read once at startup; missing var ⇒ startup error (unless empty name). Key never logged/serialised.
- `auth_style`: `bearer` (→ `Authorization: Bearer k`), `anthropic` (→ `x-api-key: k` and keep the client's `anthropic-version`, defaulting to `2023-06-01` if absent), `none`. The client's own `Authorization` / `x-api-key` / `api-key` headers are **always stripped** before forwarding (they are proxy credentials, not backend credentials).
- `health.path` empty ⇒ no active probe (passive only). Durations are Go `time.ParseDuration` strings. Defaults: interval 10s, timeout 2s, connect 3s, first_byte 300s (local models can be slow to first token on long prompts).
- `expose_models`: names clients may request directly; routed only to this backend (chain of length 1, no rename).
- Name collisions between virtual models and exposed models ⇒ startup error.
- Validation errors are fatal at startup with a clear message. Unknown JSON fields ⇒ error (`DisallowUnknownFields`) to catch typos.

## 5. Health state machine

Per backend: `healthy bool`, `failures int`, `retryAt time`, `lastErr string`.

```
           probe OK / proxied request got <500 response
   ┌────────────────────────────────────────────────┐
   ▼                                                │
HEALTHY ── failure (probe fail, connect err,  ──► UNHEALTHY(failures=n, retryAt=now+backoff(n))
           first-byte timeout, 5xx) 
```

- Start state: **healthy** (optimistic). Actively probed backends are probed once, concurrently, before the server starts serving (bounded by `health.timeout`), so a backend that is down at startup is demoted before the first request. Chosen over an extra "unknown" state: it needs no third state in the routing rule, and the cost is at most one probe timeout at startup. Passive-only backends cannot be checked up front and stay optimistic.
- On the healthy transition, the `/status` fields `last_error` and `consecutive_failures` are cleared. The internal backoff level is kept (anti-flap, below).
- `backoff(n) = min(interval * 2^(n-1), 5m)` (interval defaults 10s; for passive-only backends uses 10s base).
- Active prober: every `interval` (while healthy) or at `retryAt` (while unhealthy), `GET base_url+health.path` with the backend's auth, timeout `health.timeout`. Only 2xx counts as up for probes (a 401 means misconfigured key — mark unhealthy with lastErr "probe status 401" so `/status` surfaces it).
- Anti-flap (decided): `failures` is **not** reset by a single success. On success the backend becomes healthy and `healthySince=now`; on the next failure, if it had been healthy for ≥ `2*interval`, `failures` restarts from 0, otherwise it keeps growing. So a flapping backend's backoff keeps growing, a stable one starts fresh. Probe interval and backoff get ±10% jitter.
- A probe failure is ignored (not counted) if a proxied request to that backend succeeded within the last `interval` (a saturated llama.cpp may time out `/v1/models` while serving fine).
- Passive marking: connect/TLS error, first-byte timeout, and status 502/503/504 ⇒ failure. Other 5xx ⇒ try next backend but **do not demote**: a 500 may come from a malformed request (which must not mark the whole chain unhealthy), and a 501 means this backend does not implement the endpoint while still serving its other routes. Any response < 500 ⇒ success.
- Passive-only backends (no probe): when `retryAt` passes, the backend becomes eligible again ("half-open"): the next real request is tried on it. Known limitation: concurrent requests may all trial a half-open backend; each failed trial falls over to the next candidate transparently, so the cost is latency, not errors.
- Probes are GET only, use the backend's auth and transport, drain ≤ 64 KiB of the body, and stop on shutdown.
### Candidate ordering

For a chain `[b1, b2, b3]`: try in chain order all candidates that are healthy or whose `retryAt` has passed (half-open). If **none** are eligible, try all in chain order anyway (last resort — a stale health view must never make the proxy refuse a request a backend could have served). Each backend at most once per request.

## 6. Exact retry rule

A request moves to the next candidate **only if no response byte has been written to the client** AND one of:
1. Dial / TLS / connection error (incl. connection refused, reset before headers, DNS failure).
2. No response headers within `first_byte_timeout` (`ResponseHeaderTimeout`).
3. Upstream status `>= 500`. The upstream body is read (≤ 64 KiB) and kept as "last 5xx" for the final answer. Only 502/503/504 demote health (§5).

Status `429` and all other < 500 responses are passed through unchanged (retry-on-429 is "maybe later").

Worst case latency before failing is `sum(connect + first_byte)` over the chain; chains are short and operators can lower `first_byte_timeout`. No extra attempt cap in MVP: each backend is tried at most once per request.

When the response status is < 500 we commit: copy headers, `WriteHeader`, then stream. After commit, **never retry**. Upstream read error after commit ⇒ abort the client connection via `panic(http.ErrAbortHandler)` (after `defer upstream.Body.Close()`), so the client sees a truncated stream rather than a clean EOF that looks complete. This is the *only* abort path. Client write error / client context canceled ⇒ return quietly; the upstream request shares the client's context, so cancelling it stops generation upstream. Neither case changes backend health.

If every candidate failed: return the **last upstream 5xx** response verbatim if there was one (status + drained body), else `502` with JSON `{"error":{"message":"all backends failed: ...","type":"lmr_upstream_error"}}` (backend names + error class only, no URLs with credentials).

Client disconnect (request context canceled) ⇒ stop, no retry, don't mark backend unhealthy.

Retries are safe because the body is buffered and the upstream did not produce a response; the only duplicate-execution risk is a backend that processed the request but died before sending headers — acceptable for inference (no side effects) and documented.

## 7. Request/response handling

Hand-rolled (no `httputil.ReverseProxy`, which adds `X-Forwarded-For` and has its own retry/error semantics).

- **Concurrency/memory**: a semaphore of `max_concurrent_requests` (default 64) bounds in-flight proxied requests; waiting for a slot is capped at 30s ⇒ 503. Worst-case buffered memory ≈ `max_concurrent_requests × max_body_bytes`; documented.
- **Body**: read once with `http.MaxBytesReader(max_body_bytes)` into a growing `[]byte` (never pre-allocated from the client's `Content-Length` beyond 1 MiB) under a 60s read deadline → 413 on overflow, 408 on timeout. Each attempt uses `bytes.NewReader` with explicit `ContentLength` and `GetBody`.
- **Paths**: only paths starting `/v1/` are proxied; any path containing a `..` segment, a backslash, NUL/control characters or non-ASCII bytes (raw or decoded) ⇒ 400. Upstream URL = `base_url` (trailing slash trimmed) + request `EscapedPath()` + `?RawQuery`. All methods are forwarded.
- **Model resolution** (precedence: body field, then `X-Model` header):
  - JSON (`application/json`, or a body starting with `{` sent as empty / `text/plain` / `application/x-www-form-urlencoded`, which `curl -d` uses by default): scan the top-level object for `model` and, if `upstream_model != requested`, **splice** the new JSON string (encoded with `SetEscapeHTML(false)`) over the old value's bytes. Everything else (base64 images, key order, whitespace) is forwarded byte-for-byte. Malformed JSON is passed through for the backend to reject. More than one top-level key equal to `model` case-insensitively ⇒ 400 (model-confusion guard).
  - `multipart/form-data`: find the part named `model` with `mime/multipart`; rewrite by **splicing** that part's value in the raw buffer (all other bytes untouched, boundary unchanged). More than one `model` part ⇒ 400.
  - Otherwise / no `model` in body: `X-Model` header (400 if the body nevertheless looks like a JSON object with a `model` key). `X-Model` is removed before forwarding.
  - No model at all ⇒ 400 JSON error. Unknown model ⇒ 404 JSON error (OpenAI error shape).
- **Request headers to upstream**: clone client headers, then delete: hop-by-hop (`Connection` + every header it names, `Keep-Alive`, `Proxy-Connection`, `Proxy-Authenticate`, `Proxy-Authorization`, `TE`, `Trailer`, `Transfer-Encoding`, `Upgrade`), client credentials (`Authorization`, `x-api-key`, `api-key`), `Content-Length` (set from buffer), `X-Model`. Then add backend auth (§4). `anthropic-*` and all other headers pass through. Also stripped: `Cookie`, `Forwarded`, `X-Forwarded-*`, `X-Real-Ip`, and any header name containing `_` (WSGI aliasing). `req.Host` = upstream host. `Accept-Encoding` is forwarded as-is and the transport has `DisableCompression: true`, so `Content-Encoding` and body bytes pass through untouched.
- **Response**: copy status and headers minus hop-by-hop (incl. those named in the response `Connection` header and `Trailer`; trailers are dropped). `WriteHeader`, then flush immediately (SSE clients see the stream start), then copy loop with 32 KiB buffer and `Flush()` after every write via `http.ResponseController`.
- **Upstream transport**: one `http.Transport` per backend shared by requests and probes: dial timeout = `connect_timeout`, `ResponseHeaderTimeout` = `first_byte_timeout`, `TLSHandshakeTimeout` = connect_timeout, `MaxIdleConnsPerHost` 16, `ForceAttemptHTTP2`, `Proxy` = `ProxyFromEnvironment` only if `use_env_proxy` (default **off**: LAN backends + cloud keys must not silently go through a proxy). Client `CheckRedirect` returns `http.ErrUseLastResponse` (never follow redirects; a redirect would carry the key elsewhere). No overall timeout. `ResponseHeaderTimeout` does not cover a server that sends headers early then stalls, so a per-backend `stream_idle_timeout` (default 300s, `"0"` disables) bounds the gap between body reads after commit; the timer runs only while waiting on upstream (not while writing to a slow client). On expiry the stream is aborted like any upstream read error and the backend is demoted.
- **Server**: `ReadHeaderTimeout` 10s, `IdleTimeout` 120s, no `WriteTimeout` (streaming), `MaxHeaderBytes` 1 MiB. Plain-HTTP listener is HTTP/1.1. Go's server already rejects requests with both `Content-Length` and `Transfer-Encoding`. Shutdown on SIGINT/SIGTERM: `Shutdown` with a 10s drain, then `Close`.

## 8. Inbound auth

If `inbound_api_key_env` is set, every route except `/healthz` requires `Authorization: Bearer <key>` **or** `x-api-key: <key>` (Anthropic clients). Constant-time compare. Missing/empty env var at startup ⇒ fatal. Binding to a non-loopback address without inbound auth ⇒ **startup refusal** unless `allow_unauthenticated_remote: true`. Backends with a key and an `http://` non-loopback `base_url` ⇒ startup warning.

When inbound auth is **off**, requests (except `/healthz`) must carry a loopback `Host` and, if present, a loopback `Origin`; otherwise 403. This blocks browser CSRF and DNS-rebinding against the default `127.0.0.1` bind.

`/status` never includes `base_url`, keys or env var values: backend name, health, failures, last error class/message (errors are reduced to a generic class: connection refused, timeout, tls error, dns error, transport error, status NNN).

## 9. Logging

One line per proxied request, `log/slog` text handler to stderr:
`level=INFO msg=req method=POST path=/v1/chat/completions model=vision backend=mini-pc upstream_model=qwen2.5-vl-7b status=200 attempts=2 ttfb_ms=812 dur_ms=10233 bytes=48123 failed_attempts="gpu-laptop: connection refused"`

`failed_attempts` (present only when an attempt failed) lists every failed attempt as `backend: error class`, matching the 502 error body.
Requests rejected before proxying (401 inbound auth, 403 non-loopback host/origin, 404 unknown path) get the same line with the status and a short `err` reason. `/healthz`, `/status` and `/v1/models` are answered locally and not logged. Never logs headers, bodies or keys. Health transitions are logged (`backend=gpu-laptop healthy=false err="dial tcp: connection refused"`). `-quiet` / `"log_requests": false` disables per-request lines.

## 10. Success criteria (verifiable)

1. `go build ./...`, `go vet ./...`, `go test -race ./...` pass; `go.mod` has zero `require` lines.
2. Integration tests with in-process fake backends demonstrate: fallback on connection refused, on 5xx, on slow first byte; no retry after stream started (mid-stream drop ⇒ client sees truncated body, second backend gets 0 requests); flapping backend recovers; SSE events arrive at the client before the upstream finishes (flush proof); multipart model rewrite with file bytes intact; 20 MiB base64 JSON body forwarded intact; model rewrite per backend; `/v1/messages` with `x-api-key`/`anthropic-version`; client auth header not forwarded; inbound auth enforced; 413 on oversized body.
3. Benchmark: added latency per request vs direct call and allocations/op reported (`go test -bench . -benchmem ./internal/proxy`).
4. `/status` output contains no key material (test asserts the env value never appears in `/status` or logs).
5. Also tested: client disconnect cancels the upstream request; redirects are not followed; plain 500 tries next backend but doesn't demote health; client `Authorization` never reaches the backend; non-loopback bind without auth refused.

## 11. Milestones

- M1 config + validation + tests.
- M2 proxy core: resolve, rewrite (JSON/multipart/header), forward, stream, retry; `/v1/models`, `/healthz`, `/status`.
- M3 health: probes, passive marking, backoff, ordering.
- M4 tests (unit + integration) + benchmark.
- M5 docs (README, example configs, Qwen Code, systemd), CI, cross-compile instructions.
- M6 reviews (spec, correctness, security) recorded in `docs/REVIEW.md`.

## 12. Maybe later (not in MVP)

- Single-flight half-open trials.
- Retry on 429 / honour `Retry-After`; per-backend concurrency limits.
- Hot config reload (SIGHUP).
- Anti-flap hysteresis (N consecutive successes before healthy).
- Load-balancing (round-robin) among equal-priority backends.
- Prometheus `/metrics`.
- Wildcard / regex model routing; auto-discovery of models from `/v1/models` of backends.
- OpenAI↔Anthropic translation (explicit non-goal unless strongly requested).
