package proxy

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"syscall"
	"time"

	"github.com/zhuanyi/lite-model-router/internal/config"
	"github.com/zhuanyi/lite-model-router/internal/health"
)

const defaultAnthropicVersion = "2023-06-01"

// backend is one configured upstream: transport, credentials and health.
type backend struct {
	name         string
	base         string // base_url without trailing slash; never logged or served
	auth         string
	key          string
	probePath    string
	probeTimeout time.Duration
	streamIdle   time.Duration // 0 = no idle limit on committed responses
	client       *http.Client
	transport    *http.Transport
	h            *health.Backend
}

func newBackend(name string, bc *config.Backend, useEnvProxy bool, h *health.Backend) (*backend, error) {
	tr := &http.Transport{
		DialContext:           (&net.Dialer{Timeout: bc.Connect, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   bc.Connect,
		ResponseHeaderTimeout: bc.FirstByte,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		DisableCompression:    true, // pass Content-Encoding and bytes through untouched
	}
	if useEnvProxy {
		tr.Proxy = http.ProxyFromEnvironment
	}
	return &backend{
		name: name, base: strings.TrimRight(bc.BaseURL, "/"),
		auth: bc.AuthStyle, key: bc.APIKey(),
		probePath: bc.Health.ProbePath, probeTimeout: bc.Health.TimeoutDur,
		streamIdle: bc.StreamIdle,
		transport:  tr, h: h,
		client: &http.Client{
			Transport: tr,
			// Never follow redirects: they could carry the API key elsewhere.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// applyAuth sets the backend credential on outgoing headers (PLAN §4).
func (b *backend) applyAuth(h http.Header) {
	switch b.auth {
	case config.AuthBearer:
		if b.key != "" {
			h.Set("Authorization", "Bearer "+b.key)
		}
	case config.AuthAnthropic:
		if b.key != "" {
			h.Set("X-Api-Key", b.key)
		}
		if h.Get("Anthropic-Version") == "" {
			h.Set("Anthropic-Version", defaultAnthropicVersion)
		}
	}
}

// probe is the active health check: GET base+path, only 2xx counts as up.
func (b *backend) probe(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, b.probeTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, b.base+b.probePath, nil)
	if err != nil {
		return errors.New("invalid probe request")
	}
	b.applyAuth(req.Header)
	resp, err := b.client.Do(req)
	if err != nil {
		return errors.New(describeError(err))
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxDrain))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("probe status %d", resp.StatusCode)
	}
	return nil
}

// describeError reduces a transport error to a fixed class string. Raw net
// error text is never returned: it can carry addresses, URLs or hostnames.
func describeError(err error) string {
	var (
		dns  *net.DNSError
		cert *tls.CertificateVerificationError
		unk  x509.UnknownAuthorityError
		ne   net.Error
	)
	var ue *url.Error
	if errors.As(err, &ue) {
		err = ue.Err
	}
	msg := err.Error() // inspected, never returned
	switch {
	case errors.Is(err, syscall.ECONNREFUSED):
		return "connection refused"
	case errors.Is(err, syscall.ECONNRESET):
		return "connection reset"
	case strings.Contains(msg, "timeout awaiting response headers"):
		return "timeout waiting for response headers"
	case errors.Is(err, context.DeadlineExceeded) || (errors.As(err, &ne) && ne.Timeout()):
		return "timeout"
	case errors.As(err, &dns):
		return "dns error"
	case errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF):
		return "connection closed"
	case errors.As(err, &cert) || errors.As(err, &unk) || strings.HasPrefix(msg, "tls:") || strings.HasPrefix(msg, "x509:"):
		return "tls error"
	}
	return "transport error"
}

// Header scrubbing (PLAN §7).

var hopByHop = []string{
	"Connection", "Keep-Alive", "Proxy-Connection", "Proxy-Authenticate",
	"Proxy-Authorization", "Te", "Trailer", "Transfer-Encoding", "Upgrade",
}

// stripHopByHop removes hop-by-hop headers, including those named by Connection.
func stripHopByHop(h http.Header) {
	for _, v := range h.Values("Connection") {
		for _, name := range strings.Split(v, ",") {
			if name = strings.TrimSpace(name); name != "" {
				h.Del(name)
			}
		}
	}
	for _, k := range hopByHop {
		h.Del(k)
	}
}

// upstreamHeaders derives the outgoing request headers from the client's.
func upstreamHeaders(in http.Header) http.Header {
	h := in.Clone()
	stripHopByHop(h)
	// Client credentials are proxy credentials, never backend credentials.
	for _, k := range []string{"Authorization", "X-Api-Key", "Api-Key", "Content-Length", "X-Model",
		"Cookie", "Forwarded", "X-Real-Ip"} {
		h.Del(k)
	}
	// Client-chain identity and underscore-named headers (which some servers
	// conflate with their dashed twins) are not for the backend.
	for k := range h {
		if strings.HasPrefix(k, "X-Forwarded-") || strings.Contains(k, "_") {
			delete(h, k)
		}
	}
	if _, ok := h["User-Agent"]; !ok {
		h["User-Agent"] = []string{""} // don't advertise Go-http-client
	}
	return h
}
