package proxy

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

const benchBody = `{"model":"chat","messages":[{"role":"user","content":"hello"}],"temperature":0.2}`
const benchReply = `{"id":"x","object":"chat.completion","choices":[{"index":0,"message":{"role":"assistant","content":"hi"}}]}`

func benchBackend(b *testing.B) *fake {
	return newFake(b, "A", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, benchReply)
	})
}

func post(b *testing.B, c *http.Client, url string) {
	resp, err := c.Post(url, "application/json", strings.NewReader(benchBody))
	if err != nil {
		b.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
}

// BenchmarkDirect is the baseline: client -> backend.
func BenchmarkDirect(b *testing.B) {
	be := benchBackend(b)
	c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
	b.Cleanup(c.CloseIdleConnections)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		post(b, c, be.URL+"/v1/chat/completions")
	}
}

// BenchmarkViaProxy is client -> router (model rewrite on) -> backend.
// Allocations include the in-process backend and router.
func BenchmarkViaProxy(b *testing.B) {
	be := benchBackend(b)
	e := newEnv(b, map[string]any{"A": bk(be.URL)}, map[string]any{"chat": chain("A", "upstream")},
		map[string]any{"log_requests": false})
	c := &http.Client{Transport: &http.Transport{MaxIdleConnsPerHost: 4}}
	b.Cleanup(c.CloseIdleConnections)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		post(b, c, e.srv.URL+"/v1/chat/completions")
	}
}
