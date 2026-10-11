package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const validJSON = `{
  "listen": "127.0.0.1:4000",
  "inbound_api_key_env": "LMR_TEST_INBOUND",
  "backends": {
    "gpu": {"base_url": "http://127.0.0.1:8080", "expose_models": ["qwen-direct"],
            "health": {"path": "/v1/models", "interval": "5s", "timeout": "1s"}},
    "cloud": {"base_url": "https://api.example.com", "api_key_env": "LMR_TEST_CLOUD",
              "first_byte_timeout": "30s"}
  },
  "models": {"vision": [{"backend": "gpu", "model": "qwen-vl"}, {"backend": "cloud", "model": "gpt"}]}
}`

func setEnv(t *testing.T) {
	t.Helper()
	t.Setenv("LMR_TEST_INBOUND", "inbound-secret")
	t.Setenv("LMR_TEST_CLOUD", "cloud-secret")
}

func TestValid(t *testing.T) {
	setEnv(t)
	path := filepath.Join(t.TempDir(), "c.json")
	if err := os.WriteFile(path, []byte(validJSON), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.InboundKey() != "inbound-secret" || c.Backends["cloud"].APIKey() != "cloud-secret" {
		t.Error("secrets not resolved")
	}
	gpu, cloud := c.Backends["gpu"], c.Backends["cloud"]
	if gpu.Health.IntervalDur != 5*time.Second || gpu.Health.TimeoutDur != time.Second {
		t.Errorf("health durations: %+v", gpu.Health)
	}
	if gpu.Connect != DefaultConnectTimeout || gpu.FirstByte != DefaultFirstByteTimeout {
		t.Errorf("defaults not applied: %v %v", gpu.Connect, gpu.FirstByte)
	}
	if cloud.FirstByte != 30*time.Second || cloud.AuthStyle != AuthBearer {
		t.Errorf("cloud: %+v", cloud)
	}
	if c.MaxBodyBytes != DefaultMaxBodyBytes || !c.LogRequests || c.MaxConcurrentRequests != 64 {
		t.Errorf("top-level defaults: %+v", c)
	}
}

func TestSecretsNeverSerialised(t *testing.T) {
	setEnv(t)
	c, err := Parse([]byte(validJSON))
	if err != nil {
		t.Fatal(err)
	}
	out, _ := json.Marshal(c)
	s := fmt.Sprintf("%s %+v %v", out, c, c.Backends["cloud"])
	if strings.Contains(s, "cloud-secret") || strings.Contains(s, "inbound-secret") {
		t.Fatalf("secret leaked: %s", s)
	}
}

func TestInvalid(t *testing.T) {
	setEnv(t)
	cases := []struct{ name, json, want string }{
		{"unknown field", `{"bogus": 1, "backends": {}}`, "unknown field"},
		{"trailing data", validJSON + `{}`, "unexpected data"},
		{"bad ref", `{"backends":{"a":{"base_url":"http://127.0.0.1:1"}},"models":{"m":[{"backend":"zzz","model":"x"}]}}`, `unknown backend "zzz"`},
		{"collision", `{"backends":{"a":{"base_url":"http://127.0.0.1:1","expose_models":["m"]}},"models":{"m":[{"backend":"a","model":"x"}]}}`, "collides"},
		{"dup expose", `{"backends":{"a":{"base_url":"http://127.0.0.1:1","expose_models":["m"]},"b":{"base_url":"http://127.0.0.1:2","expose_models":["m"]}}}`, "exposed by both"},
		{"missing api env", `{"backends":{"a":{"base_url":"http://127.0.0.1:1","api_key_env":"LMR_NOPE_UNSET"}}}`, "LMR_NOPE_UNSET"},
		{"missing inbound env", `{"inbound_api_key_env":"LMR_NOPE_UNSET","backends":{"a":{"base_url":"http://127.0.0.1:1"}}}`, "LMR_NOPE_UNSET"},
		{"bad duration", `{"backends":{"a":{"base_url":"http://127.0.0.1:1","connect_timeout":"fast"}}}`, "connect_timeout"},
		{"bad scheme", `{"backends":{"a":{"base_url":"ftp://x"}}}`, "base_url"},
		{"url userinfo", `{"backends":{"a":{"base_url":"http://u:p@x"}}}`, "credentials"},
		{"bad auth style", `{"backends":{"a":{"base_url":"http://x","auth_style":"magic"}}}`, "auth_style"},
		{"empty chain", `{"backends":{"a":{"base_url":"http://x"}},"models":{"m":[]}}`, "chain is empty"},
		{"no backends", `{}`, "at least one backend"},
		{"insecure bind", `{"listen":"0.0.0.0:4000","backends":{"a":{"base_url":"http://x"}}}`, "not a loopback"},
		{"insecure bind port only", `{"listen":":4000","backends":{"a":{"base_url":"http://x"}}}`, "not a loopback"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := Parse([]byte(tc.json))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("want error containing %q, got %v", tc.want, err)
			}
		})
	}
}

func TestRemoteBindAllowed(t *testing.T) {
	setEnv(t)
	// With inbound auth: fine and no warning.
	c, err := Parse([]byte(`{"listen":"0.0.0.0:4000","inbound_api_key_env":"LMR_TEST_INBOUND","backends":{"a":{"base_url":"http://127.0.0.1:1"}}}`))
	if err != nil || len(c.Warnings) != 0 {
		t.Fatalf("err=%v warnings=%v", err, c.Warnings)
	}
	// Explicit opt-out: allowed with a warning.
	c, err = Parse([]byte(`{"listen":"0.0.0.0:4000","allow_unauthenticated_remote":true,"backends":{"a":{"base_url":"http://127.0.0.1:1"}}}`))
	if err != nil || len(c.Warnings) != 1 {
		t.Fatalf("err=%v warnings=%v", err, c.Warnings)
	}
}

func TestSetListenRechecks(t *testing.T) {
	c, err := Parse([]byte(`{"backends":{"a":{"base_url":"http://127.0.0.1:1"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := c.SetListen("0.0.0.0:9"); err == nil {
		t.Fatal("expected refusal for -listen override to a public address")
	}
	if err := c.SetListen("[::1]:9"); err != nil {
		t.Fatal(err)
	}
}

func TestHTTPKeyWarning(t *testing.T) {
	setEnv(t)
	c, err := Parse([]byte(`{"backends":{"a":{"base_url":"http://gpu.lan:1","api_key_env":"LMR_TEST_CLOUD"},
	  "b":{"base_url":"http://127.0.0.1:1","api_key_env":"LMR_TEST_CLOUD"}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], `"a"`) {
		t.Fatalf("warnings = %v", c.Warnings)
	}
	if strings.Contains(c.Warnings[0], "cloud-secret") {
		t.Fatal("warning leaks key")
	}
}
