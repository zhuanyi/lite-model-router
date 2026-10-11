package proxy

import (
	"bufio"
	"bytes"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"runtime"
	"strings"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

// --- fallback behaviour -----------------------------------------------------

func TestFallbackOnConnectionRefused(t *testing.T) {
	b := newFake(t, "B", nil)
	e := newEnv(t,
		map[string]any{"A": bk(deadURL(t)), "B": bk(b.URL)},
		map[string]any{"vision": chain("A", "m-a", "B", "m-b")}, nil)

	resp, _ := e.chat(t, "vision")
	if resp.StatusCode != 200 || resp.Header.Get("X-Backend") != "B" {
		t.Fatalf("status=%d backend=%q", resp.StatusCode, resp.Header.Get("X-Backend"))
	}
	st := e.status(t)
	if st["A"].Healthy || st["A"].LastError != "connection refused" {
		t.Fatalf("A = %+v", st["A"])
	}
	if !st["B"].Healthy {
		t.Fatal("B should be healthy")
	}
	waitFor(t, "log line", func() bool { return strings.Contains(e.logs.String(), "msg=req") })
	line := e.logs.String()
	for _, want := range []string{"method=POST", "path=/v1/chat/completions", "model=vision", "backend=B",
		"upstream_model=m-b", "status=200", "attempts=2", "ttfb_ms=", "dur_ms=", "bytes=11"} {
		if !strings.Contains(line, want) {
			t.Errorf("log line missing %q:\n%s", want, line)
		}
	}
}

func TestFallbackOn503ThenSkipsDemoted(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "down", 503) })
	b := newFake(t, "B", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL), "B": bk(b.URL)},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	if resp, _ := e.chat(t, "m"); resp.Header.Get("X-Backend") != "B" {
		t.Fatalf("first request served by %q", resp.Header.Get("X-Backend"))
	}
	if st := e.status(t); st["A"].Healthy || st["A"].LastError != "upstream status 503" {
		t.Fatalf("A = %+v", st["A"])
	}
	e.chat(t, "m")
	if a.hits.Load() != 1 {
		t.Fatalf("demoted backend A was tried again: %d hits", a.hits.Load())
	}
}

func TestFallbackOnSlowFirstByte(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
		case <-time.After(5 * time.Second):
		}
	})
	b := newFake(t, "B", nil)
	e := newEnv(t,
		map[string]any{"A": bk(a.URL, "first_byte_timeout", "100ms"), "B": bk(b.URL)},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	start := time.Now()
	resp, _ := e.chat(t, "m")
	if resp.Header.Get("X-Backend") != "B" || time.Since(start) > 3*time.Second {
		t.Fatalf("backend=%q after %v", resp.Header.Get("X-Backend"), time.Since(start))
	}
	if st := e.status(t); st["A"].Healthy || !strings.Contains(st["A"].LastError, "timeout") {
		t.Fatalf("A = %+v", st["A"])
	}
}

func TestPlain500TriesNextButDoesNotDemote(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "bad request body", 500) })
	b := newFake(t, "B", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL), "B": bk(b.URL)},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	for i := 0; i < 2; i++ {
		if resp, _ := e.chat(t, "m"); resp.Header.Get("X-Backend") != "B" {
			t.Fatalf("request %d served by %q", i, resp.Header.Get("X-Backend"))
		}
	}
	if a.hits.Load() != 2 {
		t.Fatalf("A hits = %d, want 2 (a plain 500 must not demote)", a.hits.Load())
	}
	if st := e.status(t); !st["A"].Healthy || st["A"].ConsecutiveFailures != 0 {
		t.Fatalf("A = %+v", st["A"])
	}
}

func TestAllFailLastUpstream5xxVerbatim(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) { http.Error(w, "from-a", 503) })
	b := newFake(t, "B", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Custom", "yes")
		w.WriteHeader(500)
		_, _ = io.WriteString(w, `{"error":"from-b"}`)
	})
	e := newEnv(t, map[string]any{"A": bk(a.URL), "B": bk(b.URL)},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	resp, body := e.chat(t, "m")
	if resp.StatusCode != 500 || string(body) != `{"error":"from-b"}` || resp.Header.Get("X-Custom") != "yes" {
		t.Fatalf("got %d %q", resp.StatusCode, body)
	}
}

func TestAllFailNoResponse502(t *testing.T) {
	da, db := deadURL(t), deadURL(t)
	e := newEnv(t, map[string]any{"A": bk(da), "B": bk(db)},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	resp, body := e.chat(t, "m")
	if resp.StatusCode != 502 {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	var doc struct {
		Error struct{ Message, Type string }
	}
	if err := json.Unmarshal(body, &doc); err != nil || doc.Error.Type != "lmr_upstream_error" {
		t.Fatalf("body = %s", body)
	}
	for _, u := range []string{da, db} {
		if strings.Contains(string(body), strings.TrimPrefix(u, "http://")) {
			t.Fatalf("502 body leaks backend address: %s", body)
		}
	}
	// The request log lists every failed attempt, matching the error body.
	want := "A: connection refused; B: connection refused"
	if !strings.Contains(doc.Error.Message, want) {
		t.Errorf("error body = %q", doc.Error.Message)
	}
	waitFor(t, "request log", func() bool { return strings.Contains(e.logs.String(), "msg=req") })
	if logs := e.logs.String(); !strings.Contains(logs, `failed_attempts="`+want+`"`) {
		t.Errorf("request log does not list each failed attempt:\n%s", logs)
	}
	if !strings.Contains(doc.Error.Message, "A: connection refused") {
		t.Fatalf("message = %q", doc.Error.Message)
	}
	// All unhealthy: the proxy must still try everything (last resort).
	e.chat(t, "m")
	if st := e.status(t); st["A"].ConsecutiveFailures != 1 {
		t.Fatalf("last-resort attempts inflated backoff: %+v", st["A"])
	}
}

func TestFlappingBackendRecovers(t *testing.T) {
	var down atomic.Bool
	down.Store(true)
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "down", 503)
			return
		}
		_, _ = io.WriteString(w, "{}")
	})
	b := newFake(t, "B", nil)
	e := newEnv(t,
		map[string]any{
			"A": bk(a.URL, "health", map[string]any{"path": "/health", "interval": "40ms", "timeout": "200ms"}),
			"B": bk(b.URL),
		},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	if resp, _ := e.chat(t, "m"); resp.Header.Get("X-Backend") != "B" {
		t.Fatal("expected fallback to B while A is down")
	}
	down.Store(false)
	waitFor(t, "A to be preferred again", func() bool {
		resp, _ := e.chat(t, "m")
		return resp.Header.Get("X-Backend") == "A"
	})
	if st := e.status(t); !st["A"].Healthy || st["A"].NextProbeIn == "" {
		t.Fatalf("A = %+v", st["A"])
	}
}

// --- streaming --------------------------------------------------------------

func TestMidStreamDropIsNotRetried(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1000")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "partial")
		w.(http.Flusher).Flush()
		conn, _, _ := w.(http.Hijacker).Hijack()
		conn.Close() // die mid-body
	})
	b := newFake(t, "B", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL), "B": bk(b.URL)},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("client saw a clean EOF for a truncated stream (%q)", data)
	}
	if string(data) != "partial" {
		t.Fatalf("data = %q", data)
	}
	if b.hits.Load() != 0 {
		t.Fatalf("second backend got %d requests after the stream started", b.hits.Load())
	}
	waitFor(t, "abort log", func() bool { return strings.Contains(e.logs.String(), "upstream read error") })
}

func TestSSEFlushedBeforeUpstreamFinishes(t *testing.T) {
	release := make(chan struct{})
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		select { // upstream is blocked here until the test lets go
		case <-release:
		case <-r.Context().Done():
			return
		}
		_, _ = io.WriteString(w, "data: two\n\n")
	})
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, nil)

	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m","stream":true}`))
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	br := bufio.NewReader(resp.Body)
	line := make(chan string, 1)
	go func() { l, _ := br.ReadString('\n'); line <- l }()
	select {
	case l := <-line:
		if l != "data: one\n" {
			t.Fatalf("first line = %q", l)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("first SSE event not delivered while upstream was still open")
	}
	close(release)
	rest, err := io.ReadAll(br)
	if err != nil || !strings.Contains(string(rest), "data: two") {
		t.Fatalf("rest = %q err=%v", rest, err)
	}
}

func TestClientDisconnectCancelsUpstream(t *testing.T) {
	started, cancelled := make(chan struct{}), make(chan struct{})
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		<-r.Context().Done()
		close(cancelled)
	})
	b := newFake(t, "B", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL), "B": bk(b.URL)},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		req, _ := http.NewRequestWithContext(ctx, "POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
		resp, err := e.srv.Client().Do(req)
		if err == nil {
			resp.Body.Close()
		}
		done <- err
	}()
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream never saw the request")
	}
	cancel()
	select {
	case <-cancelled:
	case <-time.After(2 * time.Second):
		t.Fatal("upstream request context was not cancelled after the client disconnected")
	}
	<-done
	waitFor(t, "499 log", func() bool { return strings.Contains(e.logs.String(), "status=499") })
	if b.hits.Load() != 0 {
		t.Fatal("client cancel must not trigger fallback")
	}
	if st := e.status(t); !st["A"].Healthy {
		t.Fatalf("client cancel demoted the backend: %+v", st["A"])
	}
}

func TestRedirectNotFollowed(t *testing.T) {
	b := newFake(t, "B", nil)
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, b.URL+"/v1/elsewhere", http.StatusFound)
	})
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, nil)

	// A non-following client observes exactly what the proxy returned.
	client := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	r2, err := client.Post(e.srv.URL+"/v1/chat/completions", "application/json", strings.NewReader(`{"model":"m"}`))
	if err != nil {
		t.Fatal(err)
	}
	r2.Body.Close()
	if r2.StatusCode != http.StatusFound || !strings.HasPrefix(r2.Header.Get("Location"), b.URL) {
		t.Fatalf("status=%d location=%q", r2.StatusCode, r2.Header.Get("Location"))
	}
	if b.hits.Load() != 0 {
		t.Fatal("the proxy followed a redirect")
	}
}

// --- request rewriting ------------------------------------------------------

func TestModelRewritePerBackend(t *testing.T) {
	var down atomic.Bool
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		if down.Load() {
			http.Error(w, "x", 503)
		}
	})
	b := newFake(t, "B", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL), "B": bk(b.URL)},
		map[string]any{"vision": chain("A", "qwen-vl-32b", "B", "qwen-vl-7b")}, nil)

	e.chat(t, "vision")
	if m := modelOf(t, a.last(t).body); m != "qwen-vl-32b" {
		t.Fatalf("A saw model %q", m)
	}
	down.Store(true)
	e.chat(t, "vision")
	if m := modelOf(t, b.last(t).body); m != "qwen-vl-7b" {
		t.Fatalf("B saw model %q", m)
	}
}

func TestUnchangedModelForwardsOriginalBytes(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL, "expose_models", []string{"direct"})}, map[string]any{}, nil)
	body := "{ \"z\": 1,\n  \"model\" : \"direct\", \"a\": \"<&>\" }"
	e.do(t, "POST", "/v1/chat/completions", body, "Content-Type", "application/json")
	if got := string(a.last(t).body); got != body {
		t.Fatalf("body changed: %q", got)
	}
}

func TestXModelHeaderRoutesAndIsStripped(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "upstream")}, nil)
	e.do(t, "POST", "/v1/audio/transcriptions", "rawbytes", "X-Model", "m", "Content-Type", "audio/wav")
	got := a.last(t)
	if string(got.body) != "rawbytes" || got.header.Get("X-Model") != "" {
		t.Fatalf("body=%q X-Model=%q", got.body, got.header.Get("X-Model"))
	}
}

func TestMultipartModelRewriteKeepsFileBytes(t *testing.T) {
	file := make([]byte, 300_000)
	_, _ = rand.Read(file)
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("model", "whisper-virtual")
	fw, _ := mw.CreateFormFile("file", "a.wav")
	_, _ = fw.Write(file)
	_ = mw.WriteField("language", "en")
	_ = mw.Close()

	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"whisper-virtual": chain("A", "whisper-1")}, nil)
	e.do(t, "POST", "/v1/audio/transcriptions", buf.String(), "Content-Type", mw.FormDataContentType())

	got := a.last(t)
	_, params, err := mime.ParseMediaType(got.header.Get("Content-Type"))
	if err != nil {
		t.Fatal(err)
	}
	mr := multipart.NewReader(bytes.NewReader(got.body), params["boundary"])
	parts := map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		parts[p.FormName()], _ = io.ReadAll(p)
	}
	if string(parts["model"]) != "whisper-1" || string(parts["language"]) != "en" || !bytes.Equal(parts["file"], file) {
		t.Fatalf("model=%q language=%q file-intact=%v", parts["model"], parts["language"], bytes.Equal(parts["file"], file))
	}
}

func TestLargeBase64JSONBodyIntact(t *testing.T) {
	// ~20 MiB of base64 built from random blocks (cheap to build, still unique content).
	block := make([]byte, 3000)
	_, _ = rand.Read(block)
	chunk := base64.StdEncoding.EncodeToString(block)
	img := "data:image/png;base64," + strings.Repeat(chunk, (20<<20)/len(chunk))
	body := `{"model":"vision","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"` + img + `"}}]}]}`

	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"vision": chain("A", "qwen-vl")}, nil)
	resp, _ := e.do(t, "POST", "/v1/chat/completions", body, "Content-Type", "application/json")
	if resp.StatusCode != 200 {
		t.Fatalf("status %d", resp.StatusCode)
	}
	want := strings.Replace(body, `"vision"`, `"qwen-vl"`, 1)
	if got := a.last(t).body; string(got) != want { // only the model value differs
		t.Fatalf("body altered: got %d bytes, want %d", len(got), len(want))
	}
}

// --- headers and credentials ------------------------------------------------

func TestAnthropicMessagesAuthStyle(t *testing.T) {
	t.Setenv("LMR_T_ANTHROPIC", "sk-ant-backend")
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{
		"A": bk(a.URL, "auth_style", "anthropic", "api_key_env", "LMR_T_ANTHROPIC", "expose_models", []string{"claude-x"}),
	}, map[string]any{}, nil)

	e.do(t, "POST", "/v1/messages", `{"model":"claude-x","max_tokens":1}`,
		"Content-Type", "application/json", "X-Api-Key", "client-key",
		"Anthropic-Version", "2023-10-01", "Anthropic-Beta", "tools-2024-04-04")
	got := a.last(t)
	if got.path != "/v1/messages" || got.header.Get("X-Api-Key") != "sk-ant-backend" ||
		got.header.Get("Anthropic-Version") != "2023-10-01" || got.header.Get("Anthropic-Beta") != "tools-2024-04-04" ||
		got.header.Get("Authorization") != "" {
		t.Fatalf("path=%s headers=%v", got.path, got.header)
	}

	e.do(t, "POST", "/v1/messages", `{"model":"claude-x"}`, "Content-Type", "application/json")
	if v := a.last(t).header.Get("Anthropic-Version"); v != "2023-06-01" {
		t.Fatalf("default anthropic-version = %q", v)
	}
}

func TestClientCredentialsNeverReachBackend(t *testing.T) {
	t.Setenv("LMR_T_BACKEND", "backend-key")
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL, "api_key_env", "LMR_T_BACKEND")},
		map[string]any{"m": chain("A", "m")}, nil)

	e.chat(t, "m",
		"Authorization", "Bearer client-secret", "X-Api-Key", "client-secret", "Api-Key", "client-secret",
		"X-Model", "ignored", "Proxy-Authorization", "Basic xxx",
		"Connection", "X-Hop", "X-Hop", "secret-hop", "X-Keep", "kept", "Anthropic-Beta", "b")
	h := a.last(t).header
	if h.Get("Authorization") != "Bearer backend-key" {
		t.Fatalf("Authorization = %q", h.Get("Authorization"))
	}
	for _, k := range []string{"X-Api-Key", "Api-Key", "X-Model", "Proxy-Authorization", "X-Hop"} {
		if h.Get(k) != "" {
			t.Errorf("header %s reached the backend: %q", k, h.Get(k))
		}
	}
	if h.Get("X-Keep") != "kept" || h.Get("Anthropic-Beta") != "b" {
		t.Errorf("ordinary headers must pass through: %v", h)
	}
	for k, v := range h {
		if strings.Contains(strings.Join(v, ""), "client-secret") {
			t.Errorf("client secret leaked in %s", k)
		}
	}

	// A backend without a key gets no Authorization at all.
	n := newFake(t, "N", nil)
	e2 := newEnv(t, map[string]any{"N": bk(n.URL)}, map[string]any{"m": chain("N", "m")}, nil)
	e2.chat(t, "m", "Authorization", "Bearer client-secret")
	if n.last(t).header.Get("Authorization") != "" {
		t.Fatal("client Authorization forwarded to a keyless backend")
	}
}

func TestResponseHopByHopStripped(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Connection", "X-Secret-Hop")
		w.Header().Set("X-Secret-Hop", "1")
		w.Header().Set("Keep-Alive", "timeout=5")
		w.Header().Set("X-Ok", "1")
	})
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, nil)
	resp, _ := e.chat(t, "m")
	if resp.Header.Get("X-Secret-Hop") != "" || resp.Header.Get("Keep-Alive") != "" || resp.Header.Get("X-Ok") != "1" {
		t.Fatalf("headers = %v", resp.Header)
	}
}

// --- local endpoints, auth, limits ------------------------------------------

func TestInboundAuthEnforced(t *testing.T) {
	t.Setenv("LMR_T_INBOUND", "inbound-key")
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")},
		map[string]any{"inbound_api_key_env": "LMR_T_INBOUND"})

	if resp, _ := e.do(t, "GET", "/healthz", ""); resp.StatusCode != 200 {
		t.Fatalf("/healthz = %d", resp.StatusCode)
	}
	for _, path := range []string{"/status", "/v1/models", "/v1/chat/completions"} {
		for _, hdr := range [][]string{nil, {"Authorization", "Bearer wrong"}, {"X-Api-Key", "wrong"}, {"Authorization", "inbound-key"}} {
			if resp, _ := e.do(t, "POST", path, `{"model":"m"}`, hdr...); resp.StatusCode != 401 {
				t.Errorf("%s with %v = %d, want 401", path, hdr, resp.StatusCode)
			}
		}
	}
	if a.hits.Load() != 0 {
		t.Fatal("unauthenticated request reached the backend")
	}
	for _, hdr := range [][]string{{"Authorization", "Bearer inbound-key"}, {"X-Api-Key", "inbound-key"}} {
		if resp, _ := e.chat(t, "m", hdr...); resp.StatusCode != 200 {
			t.Errorf("valid %v = %d", hdr, resp.StatusCode)
		}
	}
	if resp, _ := e.do(t, "GET", "/status", "", "Authorization", "Bearer inbound-key"); resp.StatusCode != 200 {
		t.Errorf("/status with key = %d", resp.StatusCode)
	}
}

func TestBodyTooLarge(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")},
		map[string]any{"max_body_bytes": 1024})
	big := `{"model":"m","pad":"` + strings.Repeat("x", 4096) + `"}`

	if resp, _ := e.do(t, "POST", "/v1/chat/completions", big, "Content-Type", "application/json"); resp.StatusCode != 413 {
		t.Fatalf("content-length body: %d", resp.StatusCode)
	}
	// Unknown length (chunked) is caught by MaxBytesReader.
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", io.NopCloser(strings.NewReader(big)))
	req.Header.Set("Content-Type", "application/json")
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 413 {
		t.Fatalf("chunked body: %d", resp.StatusCode)
	}
	if a.hits.Load() != 0 {
		t.Fatal("oversized request reached the backend")
	}
}

func TestClientErrors(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, nil)
	cases := []struct {
		name, method, path, body string
		want                     int
	}{
		{"no model", "POST", "/v1/chat/completions", `{"messages":[]}`, 400},
		{"empty body", "POST", "/v1/chat/completions", ``, 400},
		{"unknown model", "POST", "/v1/chat/completions", `{"model":"nope"}`, 404},
		{"dot-dot", "POST", "/v1/../status", `{"model":"m"}`, 400},
		{"encoded dot-dot", "POST", "/v1/%2e%2e/status", `{"model":"m"}`, 400},
		{"not under v1", "POST", "/other", `{"model":"m"}`, 404},
		{"bare v1", "GET", "/v1", ``, 404},
		{"encoded backslash", "POST", "/v1/a%5Cb", `{"model":"m"}`, 400},
		{"encoded NUL", "POST", "/v1/a%00b", `{"model":"m"}`, 400},
		{"encoded newline", "POST", "/v1/a%0Ab", `{"model":"m"}`, 400},
		{"encoded non-ascii", "POST", "/v1/caf%C3%A9", `{"model":"m"}`, 400},
		{"duplicate model key", "POST", "/v1/chat/completions", `{"model":"m","Model":"x"}`, 400},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, body := e.do(t, tc.method, tc.path, tc.body, "Content-Type", "application/json")
			if resp.StatusCode != tc.want {
				t.Fatalf("status %d, want %d (%s)", resp.StatusCode, tc.want, body)
			}
			if tc.want != 404 || tc.name == "unknown model" {
				var doc struct {
					Error struct{ Message, Type string }
				}
				if json.Unmarshal(body, &doc) != nil || doc.Error.Message == "" {
					t.Fatalf("not an error document: %s", body)
				}
			}
		})
	}
	if a.hits.Load() != 0 {
		t.Fatal("rejected requests reached the backend")
	}
}

func TestModelsListing(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL, "expose_models", []string{"direct-a"})},
		map[string]any{"vision": chain("A", "x"), "chat": chain("A", "y")}, nil)

	resp, body := e.do(t, "GET", "/v1/models", "")
	if resp.StatusCode != 200 || !strings.HasPrefix(resp.Header.Get("Content-Type"), "application/json") {
		t.Fatalf("status %d", resp.StatusCode)
	}
	var doc struct {
		Object string
		Data   []struct {
			ID, Object string
			Created    int64
			OwnedBy    string `json:"owned_by"`
		}
	}
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, m := range doc.Data {
		ids = append(ids, m.ID+":"+m.OwnedBy)
		if m.Object != "model" || m.Created == 0 {
			t.Errorf("bad entry %+v", m)
		}
	}
	want := "chat:lite-model-router direct-a:A vision:lite-model-router"
	if doc.Object != "list" || strings.Join(ids, " ") != want {
		t.Fatalf("object=%q ids=%v", doc.Object, ids)
	}
	if a.hits.Load() != 0 {
		t.Fatal("/v1/models must be answered locally")
	}
}

func TestStatusAndLogsNeverContainSecretsOrURLs(t *testing.T) {
	const backendKey, inboundKey = "sk-backend-SECRET-1", "sk-inbound-SECRET-2"
	t.Setenv("LMR_T_BKEY", backendKey)
	t.Setenv("LMR_T_IKEY", inboundKey)
	live := newFake(t, "live", nil)
	dead := deadURL(t)
	e := newEnv(t,
		map[string]any{"dead": bk(dead, "api_key_env", "LMR_T_BKEY"), "live": bk(live.URL, "api_key_env", "LMR_T_BKEY")},
		map[string]any{"m": chain("dead", "m", "live", "m")},
		map[string]any{"inbound_api_key_env": "LMR_T_IKEY"})

	auth := []string{"Authorization", "Bearer " + inboundKey}
	e.chat(t, "m", auth...)
	e.chat(t, "nope", auth...)
	_, status := e.do(t, "GET", "/status", "", auth...)
	_, models := e.do(t, "GET", "/v1/models", "", auth...)
	_, failed := e.do(t, "POST", "/v1/chat/completions", `{"model":"nope"}`, auth...)
	waitFor(t, "logs", func() bool { return strings.Count(e.logs.String(), "msg=req") >= 2 })

	logs := e.logs.String()
	for name, text := range map[string]string{"/status": string(status), "/v1/models": string(models), "error body": string(failed), "logs": logs} {
		for _, bad := range []string{backendKey, inboundKey, strings.TrimPrefix(dead, "http://"), strings.TrimPrefix(live.URL, "http://"), "Bearer"} {
			if strings.Contains(text, bad) {
				t.Errorf("%s contains %q:\n%s", name, bad, text)
			}
		}
	}
	if !strings.Contains(logs, "healthy=false") {
		t.Errorf("health transition not logged:\n%s", logs)
	}
}

func TestStatusShape(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL, "health", map[string]any{"path": "/health", "interval": "1h"})},
		map[string]any{"m": chain("A", "x")}, nil)
	waitFor(t, "prober scheduled", func() bool { return e.status(t)["A"].NextProbeIn != "" })
	_, body := e.do(t, "GET", "/status", "")
	var doc map[string]map[string]json.RawMessage
	if err := json.Unmarshal(body, &doc); err != nil {
		t.Fatal(err)
	}
	var one map[string]any
	_ = json.Unmarshal(doc["backends"]["A"], &one)
	for _, k := range []string{"healthy", "last_error", "last_change", "consecutive_failures", "next_probe_in"} {
		if _, ok := one[k]; !ok {
			t.Errorf("status missing %q: %s", k, body)
		}
	}
	if _, ok := doc["models"]["m"]; !ok {
		t.Errorf("status missing model chains: %s", body)
	}
}

func TestQuietDisablesRequestLogs(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, map[string]any{"log_requests": false})
	e.chat(t, "m")
	time.Sleep(20 * time.Millisecond)
	if strings.Contains(e.logs.String(), "msg=req") {
		t.Fatal("request logged although log_requests=false")
	}
}

func TestDescribeErrorStripsURL(t *testing.T) {
	_, err := http.Get(deadURL(t) + "/secret/path")
	if err == nil {
		t.Fatal("expected error")
	}
	if got := describeError(err); got != "connection refused" {
		t.Fatalf("describeError = %q (raw: %v)", got, err)
	}
}

// --- helpers ----------------------------------------------------------------

func mustURL(p string) *url.URL { u, _ := url.Parse(p); return u }

func modelOf(t *testing.T, body []byte) string {
	t.Helper()
	var d struct{ Model string }
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatal(fmt.Errorf("bad body %q: %w", body, err))
	}
	return d.Model
}

// --- fix round ---------------------------------------------------------------

// rawRequest writes head+partial body on a fresh connection and returns the
// response, leaving the body incomplete (a slow-loris client).
func rawRequest(t *testing.T, e *env, head string) *http.Response {
	t.Helper()
	conn, err := net.Dial("tcp", e.srv.Listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	_, _ = io.WriteString(conn, head)
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	resp, err := http.ReadResponse(bufio.NewReader(conn), nil)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestSlowBodyTimesOutWithoutPreallocating(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, nil)
	e.h.bodyTime = 150 * time.Millisecond

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	// Claims 50 MiB (under max_body_bytes) but sends 1 byte and stalls.
	resp := rawRequest(t, e, "POST /v1/chat/completions HTTP/1.1\r\nHost: 127.0.0.1\r\nContent-Length: 52428800\r\n\r\n{")
	runtime.ReadMemStats(&after)
	if resp.StatusCode != http.StatusRequestTimeout {
		t.Fatalf("status = %d, want 408", resp.StatusCode)
	}
	if grown := after.TotalAlloc - before.TotalAlloc; grown > 16<<20 {
		t.Fatalf("allocated %d MiB for a stalled upload: Content-Length was trusted", grown>>20)
	}
	waitFor(t, "semaphore slot released", func() bool { return len(e.h.sem) == 0 })
	if resp, _ := e.chat(t, "m"); resp.StatusCode != 200 {
		t.Fatalf("server unusable after timeout: %d", resp.StatusCode)
	}
}

func TestSemaphoreWaitIsBounded(t *testing.T) {
	started, release := make(chan struct{}), make(chan struct{})
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-release:
		case <-r.Context().Done():
		}
	})
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")},
		map[string]any{"max_concurrent_requests": 1})
	e.h.semWait = 100 * time.Millisecond

	done := make(chan struct{})
	go func() { defer close(done); e.chat(t, "m") }()
	<-started
	resp, body := e.chat(t, "m")
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "lmr_overloaded") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	close(release)
	<-done
}

func TestTruncated5xxDropsContentEncoding(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(500)
		_, _ = w.Write(bytes.Repeat([]byte{'x'}, maxDrain+500))
	})
	small := newFake(t, "S", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Encoding", "br")
		w.WriteHeader(500)
		_, _ = w.Write([]byte("tiny"))
	})
	e := newEnv(t, map[string]any{"A": bk(a.URL), "S": bk(small.URL)},
		map[string]any{"big": chain("A", "m"), "small": chain("S", "m")}, nil)

	resp, body := e.chat(t, "big")
	if resp.StatusCode != 500 || len(body) != maxDrain || resp.Header.Get("Content-Encoding") != "" {
		t.Fatalf("truncated: status=%d len=%d CE=%q", resp.StatusCode, len(body), resp.Header.Get("Content-Encoding"))
	}
	// Untruncated bodies keep their encoding.
	if resp, _ := e.chat(t, "small"); resp.Header.Get("Content-Encoding") != "br" {
		t.Fatal("complete 5xx body lost its Content-Encoding")
	}
}

func TestRequestBuildErrorIs500AndDoesNotDemote(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, nil)
	// A method that NewRequest rejects; the HTTP server would never deliver one,
	// so drive the handler directly.
	req := &http.Request{Method: "BAD METHOD", URL: mustURL("/v1/chat/completions"), Host: "127.0.0.1",
		Header: http.Header{"X-Model": {"m"}}, Body: http.NoBody}
	rec := httptest.NewRecorder()
	e.h.ServeHTTP(rec, req)
	if rec.Code != 500 || !strings.Contains(rec.Body.String(), "lmr_internal_error") {
		t.Fatalf("code=%d body=%s", rec.Code, rec.Body)
	}
	if st := e.status(t); !st["A"].Healthy || st["A"].ConsecutiveFailures != 0 || a.hits.Load() != 0 {
		t.Fatalf("A = %+v hits=%d", st["A"], a.hits.Load())
	}
}

func TestHostAndOriginChecksWithoutAuth(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, nil)
	get := func(host, origin string) int {
		req, _ := http.NewRequest("GET", e.srv.URL+"/v1/models", nil)
		if host != "" {
			req.Host = host
		}
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := e.srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	for _, host := range []string{"localhost", "localhost:4000", "127.0.0.1:4000", "127.9.9.9", "[::1]:4000", "[::1]"} {
		if c := get(host, ""); c != 200 {
			t.Errorf("Host %q = %d, want 200", host, c)
		}
	}
	for _, host := range []string{"evil.example", "evil.example:4000", "192.168.1.5:4000", "localhost.evil.example"} {
		if c := get(host, ""); c != 403 {
			t.Errorf("Host %q = %d, want 403 (DNS rebinding)", host, c)
		}
	}
	if c := get("", "http://localhost:3000"); c != 200 {
		t.Errorf("loopback Origin = %d", c)
	}
	for _, origin := range []string{"http://evil.example", "null", "https://127.0.0.1.evil.example"} {
		if c := get("", origin); c != 403 {
			t.Errorf("Origin %q = %d, want 403 (CSRF)", origin, c)
		}
	}
	// Liveness stays open for any Host.
	req, _ := http.NewRequest("GET", e.srv.URL+"/healthz", nil)
	req.Host = "evil.example"
	if resp, err := e.srv.Client().Do(req); err != nil || resp.StatusCode != 200 {
		t.Errorf("/healthz with foreign Host: %v %v", resp, err)
	}
}

func TestHostAndOriginNotCheckedWithAuth(t *testing.T) {
	t.Setenv("LMR_T_INBOUND", "k")
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")},
		map[string]any{"inbound_api_key_env": "LMR_T_INBOUND"})
	req, _ := http.NewRequest("GET", e.srv.URL+"/v1/models", nil)
	req.Host = "gateway.lan"
	req.Header.Set("Origin", "http://app.lan")
	req.Header.Set("Authorization", "Bearer k")
	resp, err := e.srv.Client().Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("%v %v", resp, err)
	}
	resp.Body.Close()
}

func TestIdentityHeadersStripped(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "m")}, nil)
	e.chat(t, "m", "Cookie", "session=1", "Forwarded", "for=1.2.3.4", "X-Forwarded-For", "1.2.3.4",
		"X-Forwarded-Host", "h", "X-Real-Ip", "1.2.3.4", "X-Keep", "yes")
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	req.Header["X_Under_Score"] = []string{"1"}
	if resp, err := e.srv.Client().Do(req); err == nil {
		resp.Body.Close()
	}
	for _, r := range func() []seen { a.mu.Lock(); defer a.mu.Unlock(); return a.reqs }() {
		for k := range r.header {
			if k == "Cookie" || k == "Forwarded" || k == "X-Real-Ip" || strings.HasPrefix(k, "X-Forwarded-") || strings.Contains(k, "_") {
				t.Errorf("header %q reached the backend", k)
			}
		}
	}
	if a.reqs[0].header.Get("X-Keep") != "yes" {
		t.Error("ordinary header dropped")
	}
}

func TestDescribeErrorIsAlwaysAClass(t *testing.T) {
	cases := map[string]error{
		"transport error":    &net.OpError{Op: "dial", Err: errors.New("secret 10.0.0.5:99 gpu.lan")},
		"connection refused": &net.OpError{Op: "dial", Err: syscall.ECONNREFUSED},
		"dns error":          &net.DNSError{Name: "gpu.lan", Err: "no such host"},
		"tls error":          errors.New("tls: failed to verify certificate for gpu.lan"),
		"timeout":            context.DeadlineExceeded,
	}
	for want, err := range cases {
		wrapped := &url.Error{Op: "Post", URL: "http://gpu.lan:8080/v1/x", Err: err}
		if got := describeError(wrapped); got != want {
			t.Errorf("describeError(%v) = %q, want %q", err, got, want)
		}
	}
}

func TestStalledStreamAbortedByIdleTimeout(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		_, _ = io.WriteString(w, "data: one\n\n")
		w.(http.Flusher).Flush()
		<-r.Context().Done() // stall after the first event
	})
	b := newFake(t, "B", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL, "stream_idle_timeout", "150ms"), "B": bk(b.URL)},
		map[string]any{"m": chain("A", "m", "B", "m")}, nil)

	start := time.Now()
	req, _ := http.NewRequest("POST", e.srv.URL+"/v1/chat/completions", strings.NewReader(`{"model":"m"}`))
	resp, err := e.srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err == nil {
		t.Fatalf("client saw a clean EOF for a stalled stream (%q)", data)
	}
	if string(data) != "data: one\n\n" {
		t.Fatalf("data = %q", data)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("stall detected after %v", d)
	}
	if b.hits.Load() != 0 {
		t.Fatalf("second backend got %d requests after the stream started", b.hits.Load())
	}
	waitFor(t, "idle log", func() bool { return strings.Contains(e.logs.String(), "stream idle timeout") })
	if st := e.status(t); st["A"].Healthy || st["A"].LastError != "stream idle timeout" {
		t.Fatalf("A status = %+v", st["A"])
	}
}

func TestSlowSteadyStreamNotCutByIdleTimeout(t *testing.T) {
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(200)
		for i := 0; i < 6; i++ { // 6×50ms = 300ms total, well over the 150ms idle limit
			_, _ = io.WriteString(w, "data: x\n\n")
			w.(http.Flusher).Flush()
			time.Sleep(50 * time.Millisecond)
		}
	})
	e := newEnv(t, map[string]any{"A": bk(a.URL, "stream_idle_timeout", "150ms")},
		map[string]any{"m": chain("A", "m")}, nil)
	resp, body := e.chat(t, "m")
	if resp.StatusCode != 200 || strings.Count(string(body), "data: x") != 6 {
		t.Fatalf("status %d body %q", resp.StatusCode, body)
	}
	if st := e.status(t); !st["A"].Healthy {
		t.Fatalf("A status = %+v", st["A"])
	}
}

func TestDefaultProbePathDetectsDeadBackend(t *testing.T) {
	// No health.path: the default /v1/models probe must run and demote it.
	e := newEnv(t, map[string]any{"A": map[string]any{"base_url": deadURL(t),
		"health": map[string]any{"interval": "50ms", "timeout": "50ms"}}},
		map[string]any{"m": chain("A", "x")}, nil)
	waitFor(t, "A demoted by probe", func() bool {
		st := e.status(t)["A"]
		return !st.Healthy && st.LastError == "connection refused" && st.NextProbeIn != ""
	})
}

func TestDefaultProbePathIsV1Models(t *testing.T) {
	var probes atomic.Int32
	a := newFake(t, "A", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodGet && r.URL.Path == "/v1/models" {
			probes.Add(1)
		}
	})
	newEnv(t, map[string]any{"A": map[string]any{"base_url": a.URL,
		"health": map[string]any{"interval": "50ms"}}},
		map[string]any{"m": chain("A", "x")}, nil)
	waitFor(t, "default probe", func() bool { return probes.Load() > 0 })
}

func TestNextProbeInOmittedForPassiveBackend(t *testing.T) {
	a := newFake(t, "A", nil)
	e := newEnv(t, map[string]any{"A": bk(a.URL)}, map[string]any{"m": chain("A", "x")}, nil)
	_, body := e.do(t, "GET", "/status", "")
	if strings.Contains(string(body), "next_probe_in") {
		t.Fatalf("passive-only backend reports next_probe_in: %s", body)
	}
}
