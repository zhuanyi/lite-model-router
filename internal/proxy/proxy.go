// Package proxy implements the HTTP handler: routing, model resolution and
// rewrite, ordered fallback across backends, and streaming pass-through.
package proxy

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/zhuanyi/lite-model-router/internal/config"
	"github.com/zhuanyi/lite-model-router/internal/health"
)

const (
	maxDrain = 64 << 10 // max upstream 5xx body kept / probe body drained
	copyBuf  = 32 << 10
	ownedBy  = "lite-model-router"
)

// target is one candidate of a chain: a backend plus the model name it expects.
type target struct {
	be    *backend
	model string
}

// Handler is the router. Create with New, call Start to launch the health
// probers, and serve it with net/http.
type Handler struct {
	log      *slog.Logger
	logReqs  bool
	maxBody  int64
	allowRem bool          // allow_unauthenticated_remote: skip Host/Origin checks
	bodyTime time.Duration // deadline for reading a request body
	semWait  time.Duration // max wait for a concurrency slot
	sem      chan struct{}
	keyHash  [sha256.Size]byte
	authOn   bool
	created  int64
	backends map[string]*backend
	names    []string            // sorted backend names
	routes   map[string][]target // virtual and exposed model -> chain
	virtual  map[string][]config.Target
	owner    map[string]string // model id -> owned_by for /v1/models
	bufs     sync.Pool
}

// New builds a Handler from a validated config.
func New(cfg *config.Config, logger *slog.Logger) (*Handler, error) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	h := &Handler{
		log: logger, logReqs: cfg.LogRequests, maxBody: cfg.MaxBodyBytes, allowRem: cfg.AllowUnauthenticatedRemote,
		bodyTime: 60 * time.Second, semWait: 30 * time.Second,
		sem: make(chan struct{}, cfg.MaxConcurrentRequests), created: time.Now().Unix(),
		backends: map[string]*backend{}, routes: map[string][]target{},
		virtual: cfg.Models, owner: map[string]string{},
		bufs: sync.Pool{New: func() any { b := make([]byte, copyBuf); return &b }},
	}
	if k := cfg.InboundKey(); k != "" {
		h.authOn, h.keyHash = true, sha256.Sum256([]byte(k))
	}
	for name, bc := range cfg.Backends {
		hb := health.New(name, health.Options{Interval: bc.Health.IntervalDur, Logger: logger})
		be, err := newBackend(name, bc, cfg.UseEnvProxy, hb)
		if err != nil {
			return nil, err
		}
		h.backends[name] = be
		h.names = append(h.names, name)
		for _, m := range bc.ExposeModels {
			h.routes[m] = []target{{be, m}}
			h.owner[m] = name
		}
	}
	sort.Strings(h.names)
	for vm, chain := range cfg.Models {
		ts := make([]target, len(chain))
		for i, t := range chain {
			ts[i] = target{h.backends[t.Backend], t.Model}
		}
		h.routes[vm] = ts
		h.owner[vm] = ownedBy
	}
	return h, nil
}

// Start launches one prober per backend that has a health path. Probers stop
// when ctx is cancelled.
func (h *Handler) Start(ctx context.Context) {
	for _, be := range h.backends {
		if be.probePath != "" {
			go be.h.Run(ctx, be.probe)
		}
	}
}

// Close releases idle upstream connections.
func (h *Handler) Close() {
	for _, be := range h.backends {
		be.transport.CloseIdleConnections()
	}
}

func (h *Handler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	path := r.URL.Path
	if path == "/healthz" { // liveness: always open
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "ok\n")
		return
	}
	if !h.authOn && !h.allowRem && !localRequest(r) {
		// No inbound auth: refuse browsers (CSRF) and DNS-rebinding hosts.
		writeError(w, http.StatusForbidden, "forbidden", "requests must target a loopback host when inbound auth is off")
		return
	}
	if !h.authorized(r) {
		w.Header().Set("WWW-Authenticate", "Bearer")
		writeError(w, http.StatusUnauthorized, "authentication_error", "missing or invalid API key")
		return
	}
	switch {
	case path == "/status" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.serveStatus(w)
	case path == "/v1/models" && (r.Method == http.MethodGet || r.Method == http.MethodHead):
		h.serveModels(w)
	case strings.HasPrefix(path, "/v1/"):
		h.serveProxy(w, r)
	default:
		writeError(w, http.StatusNotFound, "not_found", "not found")
	}
}

// localRequest reports whether the Host (and Origin, if sent) are loopback.
func localRequest(r *http.Request) bool {
	if !config.IsLoopbackHost(hostOnly(r.Host)) {
		return false
	}
	if o := r.Header.Get("Origin"); o != "" {
		u, err := url.Parse(o)
		return err == nil && config.IsLoopbackHost(u.Hostname())
	}
	return true
}

// hostOnly strips an optional port and IPv6 brackets.
func hostOnly(hostport string) string {
	if h, _, err := net.SplitHostPort(hostport); err == nil {
		return h
	}
	return strings.Trim(hostport, "[]")
}

// authorized checks Bearer or x-api-key against the inbound key. Both sides
// are hashed first so the comparison is constant-time regardless of length.
func (h *Handler) authorized(r *http.Request) bool {
	if !h.authOn {
		return true
	}
	match := func(s string) int {
		sum := sha256.Sum256([]byte(s))
		return subtle.ConstantTimeCompare(sum[:], h.keyHash[:])
	}
	bearer := ""
	if a := r.Header.Get("Authorization"); len(a) > 7 && strings.EqualFold(a[:7], "Bearer ") {
		bearer = a[7:]
	}
	return match(bearer)|match(r.Header.Get("X-Api-Key")) == 1
}

func (h *Handler) serveModels(w http.ResponseWriter) {
	type entry struct {
		ID      string `json:"id"`
		Object  string `json:"object"`
		Created int64  `json:"created"`
		OwnedBy string `json:"owned_by"`
	}
	data := make([]entry, 0, len(h.owner))
	for id, by := range h.owner {
		data = append(data, entry{id, "model", h.created, by})
	}
	sort.Slice(data, func(i, j int) bool { return data[i].ID < data[j].ID })
	writeJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}

func (h *Handler) serveStatus(w http.ResponseWriter) {
	backends := make(map[string]health.Snapshot, len(h.backends))
	for name, be := range h.backends {
		backends[name] = be.h.Snapshot()
	}
	exposed := map[string]string{}
	for id, by := range h.owner {
		if by != ownedBy {
			exposed[id] = by
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"backends": backends, "models": h.virtual, "exposed_models": exposed})
}

// reqLog accumulates the fields of the per-request log line. It never holds
// headers, bodies or keys.
type reqLog struct {
	method, path, model, backend, upstreamModel string
	status, attempts                            int
	ttfb, dur                                   time.Duration
	bytes                                       int64
	err                                         string
}

func (h *Handler) logRequest(l *reqLog) {
	if !h.logReqs {
		return
	}
	attrs := []any{"method", l.method, "path", l.path, "model", l.model, "backend", l.backend,
		"upstream_model", l.upstreamModel, "status", l.status, "attempts", l.attempts,
		"ttfb_ms", l.ttfb.Milliseconds(), "dur_ms", l.dur.Milliseconds(), "bytes", l.bytes}
	if l.err != "" {
		attrs = append(attrs, "err", l.err)
	}
	h.log.Info("req", attrs...)
}

// serveProxy handles one /v1/ request (PLAN §3, §6, §7).
func (h *Handler) serveProxy(w http.ResponseWriter, r *http.Request) {
	start := time.Now()
	rl := &reqLog{method: r.Method, path: r.URL.Path}
	// Deferred so the line is also written on the ErrAbortHandler panic path.
	defer func() { rl.dur = time.Since(start); h.logRequest(rl) }()
	fail := func(status int, typ, msg string) {
		rl.status = status
		writeError(w, status, typ, msg)
	}

	if !cleanPath(r) {
		fail(http.StatusBadRequest, "invalid_request_error", "invalid path")
		return
	}

	// Bound in-flight requests (and so buffered memory) before reading the
	// body, but not for ever: a full house must not queue clients unboundedly.
	wait := time.NewTimer(h.semWait)
	defer wait.Stop()
	select {
	case h.sem <- struct{}{}:
		defer func() { <-h.sem }()
	case <-r.Context().Done():
		rl.status = 499
		return
	case <-wait.C:
		w.Header().Set("Retry-After", "1")
		fail(http.StatusServiceUnavailable, "lmr_overloaded", "too many concurrent requests")
		return
	}

	body, err := h.readBody(w, r)
	if err != nil {
		var mbe *http.MaxBytesError
		if errors.Is(err, os.ErrDeadlineExceeded) {
			fail(http.StatusRequestTimeout, "invalid_request_error", "timed out reading request body")
		} else if errors.As(err, &mbe) {
			fail(http.StatusRequestEntityTooLarge, "invalid_request_error",
				fmt.Sprintf("request body exceeds %d bytes", h.maxBody))
		} else {
			fail(http.StatusBadRequest, "invalid_request_error", "could not read request body")
		}
		return
	}

	model, rewrite, err := resolveModel(r.Header, body)
	if err != nil {
		fail(http.StatusBadRequest, "invalid_request_error", err.Error())
		return
	}
	rl.model = model
	chain, ok := h.routes[model]
	if !ok {
		fail(http.StatusNotFound, "model_not_found", fmt.Sprintf("model %q is not configured", model))
		return
	}

	var (
		last    *savedResponse // last upstream 5xx, returned verbatim if all fail
		failed  []string
		bodies  = map[string][]byte{} // rewritten body per upstream model
		ordered = health.Order(chain, func(t target) bool { return t.be.h.Eligible() })
	)
	for _, t := range ordered {
		rl.attempts++
		rl.backend, rl.upstreamModel = t.be.name, t.model

		payload := body
		if rewrite != nil && t.model != model {
			if payload = bodies[t.model]; payload == nil {
				if payload, err = rewrite(t.model); err != nil {
					fail(http.StatusInternalServerError, "lmr_internal_error", "could not rewrite request body")
					return
				}
				bodies[t.model] = payload
			}
		}

		resp, err := h.send(r, t.be, payload)
		if err != nil {
			if r.Context().Err() != nil { // client went away: stop, no demotion
				rl.status = 499
				return
			}
			if errors.Is(err, errBuild) { // our bug or a hostile request, not the backend's fault
				fail(http.StatusInternalServerError, "lmr_internal_error", "could not build upstream request")
				return
			}
			msg := describeError(err)
			t.be.h.Failure(msg)
			failed = append(failed, t.be.name+": "+msg)
			continue
		}
		if resp.StatusCode >= 500 {
			last = saveResponse(resp)
			// Only gateway-class errors demote; a plain 500 may be our request's fault.
			if c := resp.StatusCode; c == 502 || c == 503 || c == 504 {
				t.be.h.Failure(fmt.Sprintf("upstream status %d", c))
			}
			failed = append(failed, fmt.Sprintf("%s: status %d", t.be.name, resp.StatusCode))
			continue
		}

		// Commit: from here on there is no retry.
		rl.ttfb = time.Since(start)
		t.be.h.Success()
		rl.status = resp.StatusCode
		h.stream(w, r, resp, t.be, rl) // may panic(http.ErrAbortHandler)
		return
	}

	if last != nil {
		rl.status = last.status
		last.write(w)
		return
	}
	fail(http.StatusBadGateway, "lmr_upstream_error", "all backends failed: "+strings.Join(failed, "; "))
}

// readBody buffers the request body once so it can be replayed on retry.
func (h *Handler) readBody(w http.ResponseWriter, r *http.Request) ([]byte, error) {
	if r.Body == nil || r.Body == http.NoBody {
		return nil, nil
	}
	// A slow client must not pin a concurrency slot (and memory) for ever.
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(h.bodyTime))
	defer func() { _ = rc.SetReadDeadline(time.Time{}) }()

	rd := http.MaxBytesReader(w, r.Body, h.maxBody)
	cl := r.ContentLength
	if cl > h.maxBody {
		return nil, &http.MaxBytesError{Limit: h.maxBody}
	}
	// Never trust Content-Length for allocation: start small, grow as bytes arrive.
	var buf bytes.Buffer
	buf.Grow(int(min(max(cl, 0), 1<<20)))
	_, err := buf.ReadFrom(rd)
	return buf.Bytes(), err
}

// cleanPath rejects traversal and anything but plain printable ASCII, checking
// both the raw and the percent-decoded path.
func cleanPath(r *http.Request) bool {
	for _, p := range []string{r.URL.Path, r.URL.EscapedPath()} {
		for i := 0; i < len(p); i++ {
			if c := p[i]; c < 0x20 || c >= 0x7f || c == '\\' {
				return false
			}
		}
	}
	for _, seg := range strings.Split(r.URL.Path, "/") {
		if seg == ".." {
			return false
		}
	}
	return true
}

var errBuild = errors.New("build upstream request")

// send performs one upstream attempt.
func (h *Handler) send(r *http.Request, be *backend, body []byte) (*http.Response, error) {
	u := be.base + r.URL.EscapedPath()
	if r.URL.RawQuery != "" {
		u += "?" + r.URL.RawQuery
	}
	// bytes.Reader gives NewRequest the exact ContentLength and a GetBody.
	req, err := http.NewRequestWithContext(r.Context(), r.Method, u, bytes.NewReader(body))
	if err != nil {
		return nil, fmt.Errorf("%w: %v", errBuild, err)
	}
	req.Header = upstreamHeaders(r.Header)
	be.applyAuth(req.Header)
	return be.client.Do(req)
}

// stream copies a committed upstream response to the client, flushing after
// the headers and after every write so SSE events are not held back.
func (h *Handler) stream(w http.ResponseWriter, r *http.Request, resp *http.Response, be *backend, rl *reqLog) {
	defer resp.Body.Close()
	// Idle-read watchdog: a backend that sent headers and then stalls would
	// otherwise hold the client forever. The timer only runs while we wait on
	// upstream (not while writing to a slow client); when it fires it closes
	// the body, which unblocks the pending Read with an error.
	var stalled atomic.Bool
	var idle *time.Timer
	if be.streamIdle > 0 {
		idle = time.AfterFunc(be.streamIdle, func() {
			stalled.Store(true)
			resp.Body.Close()
		})
		idle.Stop()
		defer idle.Stop()
	}
	hdr := resp.Header.Clone()
	stripHopByHop(hdr) // also drops Trailer; trailers are not forwarded
	dst := w.Header()
	for k, v := range hdr {
		dst[k] = v
	}
	w.WriteHeader(resp.StatusCode)
	rc := http.NewResponseController(w)
	_ = rc.Flush()

	bp := h.bufs.Get().(*[]byte)
	defer h.bufs.Put(bp)
	buf := *bp
	for {
		if idle != nil {
			idle.Reset(be.streamIdle)
		}
		n, rerr := resp.Body.Read(buf)
		if idle != nil {
			idle.Stop()
		}
		if n > 0 {
			wn, werr := w.Write(buf[:n])
			rl.bytes += int64(wn)
			if werr != nil {
				return // client gone; upstream shares its context and is cancelled
			}
			_ = rc.Flush()
		}
		switch {
		case rerr == nil:
		case rerr == io.EOF:
			return
		case r.Context().Err() != nil:
			return // client cancelled; not an upstream fault
		case stalled.Load():
			rl.err = "upstream stream idle timeout"
			be.h.Failure("stream idle timeout")
			panic(http.ErrAbortHandler)
		default:
			// Upstream died after we committed. Abort the connection so the
			// client sees a truncated stream, not a clean EOF that looks complete.
			rl.err = "upstream read error: " + describeError(rerr)
			panic(http.ErrAbortHandler)
		}
	}
}

// savedResponse is an upstream 5xx kept so it can be returned if every
// candidate fails.
type savedResponse struct {
	status int
	header http.Header
	body   []byte
}

func saveResponse(resp *http.Response) *savedResponse {
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxDrain+1))
	hdr := resp.Header.Clone()
	stripHopByHop(hdr)
	if len(body) > maxDrain {
		body = body[:maxDrain]
		hdr.Del("Content-Encoding") // a truncated gzip/br stream would be undecodable garbage
	}
	return &savedResponse{status: resp.StatusCode, header: hdr, body: body}
}

func (s *savedResponse) write(w http.ResponseWriter) {
	dst := w.Header()
	for k, v := range s.header {
		dst[k] = v
	}
	dst.Set("Content-Length", fmt.Sprint(len(s.body))) // body may have been truncated
	w.WriteHeader(s.status)
	_, _ = w.Write(s.body)
}

// writeError emits an OpenAI-shaped JSON error.
func writeError(w http.ResponseWriter, status int, typ, msg string) {
	writeJSON(w, status, map[string]any{"error": map[string]any{"message": msg, "type": typ}})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	enc := json.NewEncoder(w)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	_ = enc.Encode(v)
}
