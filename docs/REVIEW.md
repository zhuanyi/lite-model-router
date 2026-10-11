# Review record

All reviews were done by independent Claude subagents that had no part in writing the code they reviewed. Findings were either fixed or explicitly accepted. As the protocol allows, one fix round followed the code reviews.

## 1. Spec review of `docs/PLAN.md` (before implementation)

There were 19 findings: 3 blockers, 9 major and 7 minor. They were applied to the plan as follows.

| # | Finding | Resolution |
|---|---|---|
| 1–2 | The behaviour for a mid-stream abort and for a client disconnect was not specified. | `panic(http.ErrAbortHandler)` is used **only** when reading from upstream fails after the response is committed. If the client goes away, the handler returns quietly and the upstream request is cancelled through the shared context. Headers are flushed immediately after `WriteHeader`. |
| 3 | The plan contained undecided "thinking-out-loud" passages. | Decided: `Accept-Encoding` is forwarded with `DisableCompression`, and only a 2xx probe counts as up. |
| 4 | A single success reset the backoff, so a flapping backend never backed off. | Backoff now resets only after the backend has stayed healthy for at least `2*interval`. ±10% jitter was added. |
| 5 | A half-open backend could take a pile-on of trial requests. | Accepted and documented: the cost is extra latency, not errors. |
| 6 | A plain 500 caused by a bad request would demote the whole chain. | Only connection errors, timeouts and 502/503/504 demote a backend. A 500 still moves on to the next backend. |
| 7 | Timeout defaults conflicted, and the worst-case latency was not stated. | One set of defaults is used (connect 3s, first byte 300s), and the worst case is documented. |
| 8 | Memory was unbounded across concurrent buffered bodies. | `max_concurrent_requests` semaphore, explicit `ContentLength` on each attempt, and the original bytes are reused when no rewrite is needed. |
| 9 | Re-encoding multipart bodies is risky. | Replaced by splicing only the value of the `model` field. |
| 10–11 | Header handling (hop-by-hop, `anthropic-*`, `Host`) was incomplete. | The header list was completed, the proxy is hand-rolled with no `X-Forwarded-For`, and all `anthropic-*` headers are passed through. |
| 12 | Security defaults were weak. | A non-loopback bind without auth now **refuses to start**. `/status` omits `base_url`. `use_env_proxy` defaults to off. Redirects are never followed. A warning is logged for a key sent over `http://`. |
| 13–17 | Path traversal, the `/v1/models` shape, probe body draining, and drain on shutdown. | All applied. |

## 2. Code review A: correctness, concurrency and streaming

| # | Sev | Finding | Resolution |
|---|---|---|---|
| A1 | med | Probe hot loop: a probe failure on an unhealthy backend was suppressed by the "recent proxy success" rule, so `retryAt` never advanced and probes ran every 10 ms. | Fixed. The suppression now applies only while the backend is healthy. |
| A2 | med | After a passive state change, the prober kept sleeping on its old timer for up to 5 minutes. | Fixed with a wake channel. |
| A3 | med | No deadline on reading the body, and the buffer was preallocated from the client's `Content-Length`. | Fixed: incremental growth, a 60 s body read deadline, and a bounded 30 s wait for the semaphore that returns 503. |
| A4 | low | A truncated gzip 5xx body was returned with a stale `Content-Encoding`. | Fixed. |
| A5 | low | The multipart boundary match was missing the RFC 2046 check on the bytes that follow the boundary. | Fixed. |
| A6 | low | An error while building the request demoted the backend. | Fixed: it now returns 500 without demoting. |
| A7 | low | A second SIGINT during the drain was swallowed. | Fixed. |

Checked with no finding: health locking under `-race`, backoff and ordering, the retry-before-commit rule, body replay, the streaming flush/abort/close paths, the semaphore release paths, the JSON splice (nested and escaped `model` keys) and shutdown.

## 3. Code review B: security

| # | Sev | Finding | Resolution |
|---|---|---|---|
| B1 | med | Slow or oversized bodies could pin about 4 GiB of memory and starve the semaphore. | Fixed (same fix as A3). |
| B2 | med | Model confusion: `{"model":"m","Model":"x"}`, a duplicate `model` field, or `X-Model` combined with a JSON body that has a model all let a backend read a different model from the one that was routed. | Fixed: such requests are rejected with 400. |
| B3 | med | Browser CSRF and DNS rebinding against the unauthenticated loopback default. | Fixed: when inbound auth is off, `Host` must be loopback and any `Origin` must be loopback, otherwise 403. |
| B4 | low | `String()`/`GoString()` on the config used pointer receivers, so printing a struct by value could leak keys. | Fixed with value receivers and a test. |
| B5 | low | `Cookie`, `X-Forwarded-*` and headers with underscores (WSGI header aliasing) were forwarded. | Fixed: they are stripped. |
| B6 | low | Response headers on a 5xx or 3xx are passed through verbatim. | **Accepted.** Transparency is a design goal, and backends come only from config. |
| B7 | low | Raw network error text (for example a proxy address) appeared in `/status`, the 502 body and the logs. | Fixed: only generic error classes are shown. |
| B8 | low | Paths containing `\` or NUL were forwarded. | Fixed: backslashes, control characters and non-ASCII bytes are rejected. |
| B9 | info | Env secrets were not trimmed, and the auth warning fired for the wrong address after a `-listen` override. | Fixed. |

Checked with no finding:
- SSRF: the upstream host always comes from config. Traversal variants (encoded and double-encoded), absolute-form request targets and `Host` overrides were all tested.
- Redirects are not followed.
- The environment proxy is off by default.
- Hop-by-hop headers are stripped, and smuggling via `Content-Length` plus `Transfer-Encoding` is not possible because bodies are fully buffered and re-framed.
- Client credentials are never forwarded.
- Inbound auth is constant-time and covers `/status` and `/v1/models`.
- The bind-address check covers `:4000`, `0.0.0.0`, `[::]`, hostnames and `-listen`.
- Logs contain no headers, bodies or keys.
