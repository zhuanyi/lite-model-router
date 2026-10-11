package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/zhuanyi/lite-model-router/internal/config"
	"github.com/zhuanyi/lite-model-router/internal/health"
)

// syncBuf is a goroutine-safe log sink.
type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuf) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// seen is a request as received by a fake backend.
type seen struct {
	method, path string
	header       http.Header
	body         []byte
}

// fake is an httptest backend that counts and records the requests it gets,
// except health probes (path /health), which are served by the same handler.
type fake struct {
	*httptest.Server
	name string
	hits atomic.Int32
	mu   sync.Mutex
	reqs []seen
}

func newFake(tb testing.TB, name string, h http.HandlerFunc) *fake {
	tb.Helper()
	f := &fake{name: name}
	if h == nil {
		h = func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			_, _ = io.WriteString(w, `{"ok":true}`)
		}
	}
	f.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		r.Body = io.NopCloser(bytes.NewReader(body))
		if r.URL.Path != "/health" {
			f.hits.Add(1)
			f.mu.Lock()
			f.reqs = append(f.reqs, seen{r.Method, r.URL.RequestURI(), r.Header.Clone(), body})
			f.mu.Unlock()
		}
		w.Header().Set("X-Backend", name)
		h(w, r)
	}))
	tb.Cleanup(f.Close)
	return f
}

func (f *fake) last(tb testing.TB) seen {
	tb.Helper()
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.reqs) == 0 {
		tb.Fatalf("backend %s received no requests", f.name)
	}
	return f.reqs[len(f.reqs)-1]
}

// deadURL returns a URL whose port refuses connections.
func deadURL(tb testing.TB) string {
	tb.Helper()
	s := httptest.NewServer(http.NotFoundHandler())
	u := s.URL
	s.Close()
	return u
}

// bk builds a backend config entry from base_url and extra key/value pairs.
func bk(url string, kv ...any) map[string]any {
	m := map[string]any{"base_url": url}
	for i := 0; i < len(kv); i += 2 {
		m[kv[i].(string)] = kv[i+1]
	}
	if _, ok := m["health"]; !ok {
		m["health"] = map[string]any{"path": ""} // passive only unless a test opts in
	}
	return m
}

// chain builds a model chain from backend, model, backend, model, ...
func chain(pairs ...string) []map[string]string {
	var out []map[string]string
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, map[string]string{"backend": pairs[i], "model": pairs[i+1]})
	}
	return out
}

// env is a running router under test.
type env struct {
	srv  *httptest.Server
	h    *Handler
	logs *syncBuf
}

// newEnv builds a router from a config assembled in Go and serves it.
func newEnv(tb testing.TB, backends map[string]any, models map[string]any, extra map[string]any) *env {
	tb.Helper()
	doc := map[string]any{"backends": backends, "models": models}
	for k, v := range extra {
		doc[k] = v
	}
	raw, err := json.Marshal(doc)
	if err != nil {
		tb.Fatal(err)
	}
	cfg, err := config.Parse(raw)
	if err != nil {
		tb.Fatal(err)
	}
	logs := &syncBuf{}
	h, err := New(cfg, slog.New(slog.NewTextHandler(logs, nil)))
	if err != nil {
		tb.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.Start(ctx)
	srv := httptest.NewServer(h)
	tb.Cleanup(func() { cancel(); srv.Close(); h.Close() })
	return &env{srv: srv, h: h, logs: logs}
}

// do sends a request through the router with a short overall timeout.
func (e *env) do(tb testing.TB, method, path, body string, hdr ...string) (*http.Response, []byte) {
	tb.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		tb.Fatal(err)
	}
	for i := 0; i < len(hdr); i += 2 {
		req.Header.Set(hdr[i], hdr[i+1])
	}
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		tb.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		tb.Fatal(err)
	}
	return resp, data
}

// chat posts a minimal chat completion for model.
func (e *env) chat(tb testing.TB, model string, hdr ...string) (*http.Response, []byte) {
	tb.Helper()
	hdr = append([]string{"Content-Type", "application/json"}, hdr...)
	return e.do(tb, "POST", "/v1/chat/completions", `{"model":"`+model+`","messages":[]}`, hdr...)
}

func (e *env) status(tb testing.TB) map[string]health.Snapshot {
	tb.Helper()
	_, data := e.do(tb, "GET", "/status", "")
	var doc struct {
		Backends map[string]health.Snapshot `json:"backends"`
	}
	if err := json.Unmarshal(data, &doc); err != nil {
		tb.Fatalf("bad /status: %v\n%s", err, data)
	}
	return doc.Backends
}

func waitFor(tb testing.TB, what string, cond func() bool) {
	tb.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			tb.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(5 * time.Millisecond)
	}
}
