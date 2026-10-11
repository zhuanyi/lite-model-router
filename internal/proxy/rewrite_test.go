package proxy

import (
	"bytes"
	"crypto/rand"
	"encoding/json"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"testing"
)

func jsonHeader() http.Header {
	return http.Header{"Content-Type": {"application/json"}}
}

func TestJSONRewritePreservesOtherBytes(t *testing.T) {
	// base64 with +/=, `<>&`, odd spacing and key order must survive byte-for-byte.
	img := "data:image/png;base64," + strings.Repeat("QUJD+/Zz==", 1000)
	body := []byte("{ \"z\": 1.50 ,\n \"image\":\"" + img + "\", \"model\" :  \"vision\" ,\"odd\":\"a<b>&c \\\" \\u2028\"}")

	name, rw, err := resolveModel(jsonHeader(), body)
	if err != nil || name != "vision" || rw == nil {
		t.Fatalf("name=%q err=%v", name, err)
	}
	out, err := rw("qwen2.5-vl <7b> & co")
	if err != nil {
		t.Fatal(err)
	}
	want := bytes.Replace(body, []byte(`"vision"`), []byte(`"qwen2.5-vl <7b> & co"`), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("only the model value may change:\n got %.120s\nwant %.120s", out, want)
	}
	if !json.Valid(out) {
		t.Fatal("rewritten body is not valid JSON")
	}
}

func TestFindJSONModel(t *testing.T) {
	cases := []struct {
		name, in, want string // want: raw value, "" = not found, "ERR" = malformed
	}{
		{"simple", `{"model":"a"}`, `"a"`},
		{"nested ignored", `{"messages":[{"model":"x"}],"o":{"model":"y"},"model":"a"}`, `"a"`},
		{"model inside string", `{"t":"\"model\":\"x\"","model":"a"}`, `"a"`},
		{"escaped quote then backslash", `{"t":"a\\","model":"a"}`, `"a"`},
		{"escaped key", `{"mod\u0065l":"a"}`, `"a"`},
		{"last duplicate wins", `{"model":"a","model":"b"}`, `"b"`},
		{"non-string value", `{"model": 12 }`, `12`},
		{"null", `{"model":null}`, `null`},
		{"empty object", `{}`, ``},
		{"array body", `[1]`, `ERR`},
		{"truncated", `{"model":"a`, `ERR`},
		{"missing colon", `{"model" "a"}`, `ERR`},
		{"unterminated nesting", `{"a":[1,2`, `ERR`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s, e, found, err := findJSONModel([]byte(tc.in))
			switch tc.want {
			case "ERR":
				if err == nil {
					t.Fatal("expected error")
				}
			case "":
				if err != nil || found {
					t.Fatalf("found=%v err=%v", found, err)
				}
			default:
				if err != nil || !found || tc.in[s:e] != tc.want {
					t.Fatalf("got %q found=%v err=%v", tc.in[s:e], found, err)
				}
			}
		})
	}
}

func TestJSONResolveVariants(t *testing.T) {
	cases := []struct {
		name    string
		hdr     http.Header
		body    string
		want    string
		wantErr string
	}{
		{"json", jsonHeader(), `{"model":"a"}`, "a", ""},
		{"no content type, brace", http.Header{}, ` {"model":"a"}`, "a", ""},
		{"json+suffix", http.Header{"Content-Type": {"application/vnd.x+json"}}, `{"model":"a"}`, "a", ""},
		{"body beats header", http.Header{"Content-Type": {"application/json"}, "X-Model": {"h"}}, `{"model":"a"}`, "a", ""},
		{"header fallback", http.Header{"Content-Type": {"application/json"}, "X-Model": {"h"}}, `{"x":1}`, "h", ""},
		{"header only", http.Header{"X-Model": {"h"}}, ``, "h", ""},
		{"none", jsonHeader(), `{"x":1}`, "", "no model"},
		{"non-string", jsonHeader(), `{"model":3}`, "", "must be a string"},
		{"bad json", jsonHeader(), `{"model":`, "", "valid JSON"},
		{"curl default type", http.Header{"Content-Type": {"application/x-www-form-urlencoded"}}, `{"model":"a"}`, "a", ""},
		{"binary body ignored", http.Header{"Content-Type": {"application/octet-stream"}}, `{"model":"a"}`, "", "no model"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := resolveModel(tc.hdr, []byte(tc.body))
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want %q", err, tc.wantErr)
				}
				return
			}
			if err != nil || got != tc.want {
				t.Fatalf("got %q err %v", got, err)
			}
		})
	}
}

func TestHeaderModelHasNoRewriter(t *testing.T) {
	_, rw, err := resolveModel(http.Header{"X-Model": {"h"}}, nil)
	if err != nil || rw != nil {
		t.Fatalf("rw=%v err=%v", rw != nil, err)
	}
}

func buildMultipart(t *testing.T, model string, file []byte) (body []byte, ctype string) {
	t.Helper()
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "describe")
	fw, _ := mw.CreateFormFile("file", "pic.png")
	_, _ = fw.Write(file)
	_ = mw.WriteField("model", model)
	_ = mw.WriteField("temperature", "0.2")
	_ = mw.Close()
	return buf.Bytes(), mw.FormDataContentType()
}

func TestMultipartSplice(t *testing.T) {
	// Binary payload that includes CRLF and dashes, but never the boundary.
	file := make([]byte, 100_000)
	_, _ = rand.Read(file)
	copy(file[100:], "\r\n--not-the-boundary\r\n\r\n")

	body, ctype := buildMultipart(t, "vision", file)
	h := http.Header{"Content-Type": {ctype}}
	name, rw, err := resolveModel(h, body)
	if err != nil || name != "vision" {
		t.Fatalf("name=%q err=%v", name, err)
	}
	out, err := rw("qwen-vl-7b")
	if err != nil {
		t.Fatal(err)
	}

	// Every byte outside the model value is untouched.
	s, e, _, _ := findMultipartModel(body, boundaryOf(t, ctype))
	if !bytes.Equal(out[:s], body[:s]) || !bytes.Equal(out[s+len("qwen-vl-7b"):], body[e:]) {
		t.Fatal("bytes outside the model value changed")
	}

	// And an independent parser agrees on the result.
	mr := multipart.NewReader(bytes.NewReader(out), boundaryOf(t, ctype))
	seen := map[string][]byte{}
	for {
		p, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		seen[p.FormName()], _ = io.ReadAll(p)
	}
	if string(seen["model"]) != "qwen-vl-7b" || !bytes.Equal(seen["file"], file) ||
		string(seen["prompt"]) != "describe" || string(seen["temperature"]) != "0.2" {
		t.Fatalf("parts after splice: model=%q file-equal=%v", seen["model"], bytes.Equal(seen["file"], file))
	}
}

func TestMultipartSpliceUnescaped(t *testing.T) {
	body, ctype := buildMultipart(t, "m", []byte("x"))
	_, rw, _ := resolveModel(http.Header{"Content-Type": {ctype}}, body)
	out, _ := rw("a<b>&c")
	if !bytes.Contains(out, []byte("\r\n\r\na<b>&c\r\n--")) {
		t.Fatal("model value altered")
	}
}

func TestMultipartNoModelFallsBackToHeader(t *testing.T) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	_ = mw.WriteField("prompt", "x")
	_ = mw.Close()
	h := http.Header{"Content-Type": {mw.FormDataContentType()}, "X-Model": {"hdr"}}
	name, rw, err := resolveModel(h, buf.Bytes())
	if err != nil || name != "hdr" || rw != nil {
		t.Fatalf("name=%q rw=%v err=%v", name, rw != nil, err)
	}
}

func TestMultipartMalformed(t *testing.T) {
	h := http.Header{"Content-Type": {"multipart/form-data; boundary=xyz"}}
	if _, _, err := resolveModel(h, []byte("--xyz\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\nvalue-never-closed")); err == nil {
		t.Fatal("expected error for unterminated part")
	}
	if _, _, err := resolveModel(http.Header{"Content-Type": {"multipart/form-data"}}, []byte("x")); err == nil {
		t.Fatal("expected error for missing boundary")
	}
}

func boundaryOf(t *testing.T, ctype string) string {
	t.Helper()
	_, after, ok := strings.Cut(ctype, "boundary=")
	if !ok {
		t.Fatal("no boundary")
	}
	return after
}
