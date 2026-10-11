// Package config loads and validates the lite-model-router JSON configuration.
//
// Secrets referenced by api_key_env / inbound_api_key_env are read from the
// environment once, at load time, into unexported fields so they can never be
// serialised (JSON, %v, status pages) by accident.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"os"
	"sort"
	"strings"
	"time"
)

// Defaults (PLAN §4).
const (
	DefaultListen           = "127.0.0.1:4000"
	DefaultMaxBodyBytes     = 64 << 20
	DefaultMaxConcurrent    = 64
	DefaultProbeInterval    = 10 * time.Second
	DefaultProbeTimeout     = 2 * time.Second
	DefaultConnectTimeout   = 3 * time.Second
	DefaultFirstByteTimeout = 300 * time.Second
)

// Auth styles a backend can use.
const (
	AuthBearer    = "bearer"
	AuthAnthropic = "anthropic"
	AuthNone      = "none"
)

// Config is the top-level configuration.
type Config struct {
	Listen                     string              `json:"listen"`
	InboundAPIKeyEnv           string              `json:"inbound_api_key_env"`
	MaxBodyBytes               int64               `json:"max_body_bytes"`
	LogRequests                bool                `json:"log_requests"`
	MaxConcurrentRequests      int                 `json:"max_concurrent_requests"`
	UseEnvProxy                bool                `json:"use_env_proxy"`
	AllowUnauthenticatedRemote bool                `json:"allow_unauthenticated_remote"`
	Backends                   map[string]*Backend `json:"backends"`
	Models                     map[string][]Target `json:"models"`

	// Warnings are non-fatal findings the caller should log.
	Warnings []string `json:"-"`

	inboundKey string // resolved from InboundAPIKeyEnv
}

// Target is one hop of a virtual model's fallback chain.
type Target struct {
	Backend string `json:"backend"`
	Model   string `json:"model"`
}

// Backend describes one upstream server.
type Backend struct {
	BaseURL          string   `json:"base_url"`
	APIKeyEnv        string   `json:"api_key_env"`
	AuthStyle        string   `json:"auth_style"`
	Health           Health   `json:"health"`
	ConnectTimeout   string   `json:"connect_timeout"`
	FirstByteTimeout string   `json:"first_byte_timeout"`
	ExposeModels     []string `json:"expose_models"`

	// Parsed durations (defaults applied).
	Connect   time.Duration `json:"-"`
	FirstByte time.Duration `json:"-"`

	apiKey string // resolved from APIKeyEnv
}

// Health configures active probing of a backend.
type Health struct {
	Path     string `json:"path"`
	Interval string `json:"interval"`
	Timeout  string `json:"timeout"`

	IntervalDur time.Duration `json:"-"`
	TimeoutDur  time.Duration `json:"-"`
}

// String and GoString keep resolved secrets out of fmt output (%v, %+v, %#v).
func (b *Backend) String() string   { return "backend(" + b.BaseURL + ")" }
func (b *Backend) GoString() string { return b.String() }
func (c *Config) String() string    { return "config(listen=" + c.Listen + ")" }
func (c *Config) GoString() string  { return c.String() }

// APIKey returns the backend credential resolved from the environment.
func (b *Backend) APIKey() string { return b.apiKey }

// InboundKey returns the inbound credential ("" when auth is disabled).
func (c *Config) InboundKey() string { return c.inboundKey }

// Load reads, decodes and validates the config file at path.
func Load(path string) (*Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return Parse(data)
}

// Parse decodes and validates a config document.
func Parse(data []byte) (*Config, error) {
	cfg := &Config{
		Listen:                DefaultListen,
		MaxBodyBytes:          DefaultMaxBodyBytes,
		LogRequests:           true,
		MaxConcurrentRequests: DefaultMaxConcurrent,
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if _, err := dec.Token(); err != io.EOF {
		return nil, errors.New("config: unexpected data after JSON document")
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// SetListen overrides the listen address (-listen flag) and re-checks the
// bind-safety rule against the new address.
func (c *Config) SetListen(addr string) error {
	c.Listen = addr
	if msg := c.checkListen(); msg != "" {
		return errors.New("config: " + msg)
	}
	return nil
}

func (c *Config) checkListen() string {
	host, _, err := net.SplitHostPort(c.Listen)
	if err != nil {
		return fmt.Sprintf("listen %q: %v", c.Listen, err)
	}
	if !isLoopbackHost(host) && c.inboundKey == "" {
		if !c.AllowUnauthenticatedRemote {
			return fmt.Sprintf("listen %q is not a loopback address and inbound_api_key_env is not set; "+
				"set inbound_api_key_env or allow_unauthenticated_remote=true", c.Listen)
		}
		c.Warnings = append(c.Warnings, fmt.Sprintf("listening on %q without inbound authentication", c.Listen))
	}
	return ""
}

func (c *Config) validate() error {
	var errs []string
	bad := func(format string, a ...any) { errs = append(errs, fmt.Sprintf(format, a...)) }

	if c.MaxBodyBytes <= 0 {
		c.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if c.MaxConcurrentRequests <= 0 {
		c.MaxConcurrentRequests = DefaultMaxConcurrent
	}
	if c.InboundAPIKeyEnv != "" {
		if c.inboundKey = os.Getenv(c.InboundAPIKeyEnv); c.inboundKey == "" {
			bad("inbound_api_key_env: environment variable %q is not set or empty", c.InboundAPIKeyEnv)
		}
	}
	if len(c.Backends) == 0 {
		bad("backends: at least one backend is required")
	}

	exposed := map[string]string{} // exposed model -> backend
	for _, name := range sortedKeys(c.Backends) {
		b := c.Backends[name]
		if b == nil {
			bad("backends.%s: must be an object", name)
			continue
		}
		c.validateBackend(name, b, bad)
		for _, m := range b.ExposeModels {
			if m == "" {
				bad("backends.%s.expose_models: empty model name", name)
			} else if prev, dup := exposed[m]; dup {
				bad("expose_models: %q is exposed by both %q and %q", m, prev, name)
			} else {
				exposed[m] = name
			}
		}
	}

	for _, vm := range sortedKeys(c.Models) {
		chain := c.Models[vm]
		if vm == "" {
			bad("models: empty model name")
		}
		if owner, clash := exposed[vm]; clash {
			bad("models.%s: name collides with a model exposed by backend %q", vm, owner)
		}
		if len(chain) == 0 {
			bad("models.%s: chain is empty", vm)
		}
		for i, t := range chain {
			if _, ok := c.Backends[t.Backend]; !ok {
				bad("models.%s[%d]: unknown backend %q", vm, i, t.Backend)
			}
			if t.Model == "" {
				bad("models.%s[%d]: model is empty", vm, i)
			}
		}
	}

	if msg := c.checkListen(); msg != "" {
		bad("%s", msg)
	}
	if len(errs) > 0 {
		return errors.New("config: " + strings.Join(errs, "; "))
	}
	return nil
}

func (c *Config) validateBackend(name string, b *Backend, bad func(string, ...any)) {
	where := "backends." + name
	u, err := url.Parse(b.BaseURL)
	switch {
	case err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "":
		bad("%s.base_url: must be an http(s) URL with a host", where)
	case u.User != nil:
		bad("%s.base_url: credentials in the URL are not allowed; use api_key_env", where)
	case u.RawQuery != "" || u.Fragment != "":
		bad("%s.base_url: query and fragment are not allowed", where)
	}

	switch b.AuthStyle {
	case "":
		b.AuthStyle = AuthBearer
	case AuthBearer, AuthAnthropic, AuthNone:
	default:
		bad("%s.auth_style: %q is not one of bearer, anthropic, none", where, b.AuthStyle)
	}

	if b.APIKeyEnv != "" {
		if b.apiKey = os.Getenv(b.APIKeyEnv); b.apiKey == "" {
			bad("%s.api_key_env: environment variable %q is not set or empty", where, b.APIKeyEnv)
		}
	}
	if b.apiKey != "" && u != nil && u.Scheme == "http" && !isLoopbackHost(u.Hostname()) {
		c.Warnings = append(c.Warnings, fmt.Sprintf(
			"backend %q sends its API key over plain http to a non-loopback host", name))
	}

	if p := b.Health.Path; p != "" && !strings.HasPrefix(p, "/") {
		bad("%s.health.path: must start with /", where)
	}
	b.Health.IntervalDur = parseDur(where+".health.interval", b.Health.Interval, DefaultProbeInterval, bad)
	b.Health.TimeoutDur = parseDur(where+".health.timeout", b.Health.Timeout, DefaultProbeTimeout, bad)
	b.Connect = parseDur(where+".connect_timeout", b.ConnectTimeout, DefaultConnectTimeout, bad)
	b.FirstByte = parseDur(where+".first_byte_timeout", b.FirstByteTimeout, DefaultFirstByteTimeout, bad)
}

// parseDur parses a time.ParseDuration string; "" yields def.
func parseDur(where, s string, def time.Duration, bad func(string, ...any)) time.Duration {
	if s == "" {
		return def
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		bad("%s: %q is not a positive duration like \"10s\"", where, s)
		return def
	}
	return d
}

// isLoopbackHost reports whether host is localhost or a loopback IP literal.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	if i := strings.IndexByte(host, '%'); i >= 0 { // IPv6 zone
		host = host[:i]
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
