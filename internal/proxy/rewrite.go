package proxy

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/textproto"
	"strings"
)

var (
	errMalformed = errors.New("request body is not a valid JSON object")
	errAmbiguous = errors.New(`request has more than one "model" field`)
	errConflict  = errors.New(`X-Model conflicts with a "model" field in the request body`)
)

var errNoModel = errors.New(`no model specified: set "model" in the JSON body or multipart form, or send an X-Model header`)

// rewriter returns a copy of the request body with the model replaced.
type rewriter func(model string) ([]byte, error)

// sniffJSON lists content types for which a body starting with "{" is treated
// as JSON (curl -d defaults to x-www-form-urlencoded, for instance).
var sniffJSON = map[string]bool{"": true, "application/x-www-form-urlencoded": true, "text/plain": true}

// resolveModel finds the requested model: body field first (JSON or
// multipart), then the X-Model header. The returned rewriter is nil when the
// model came from the header (the body is then forwarded untouched).
func resolveModel(h http.Header, body []byte) (string, rewriter, error) {
	header := strings.TrimSpace(h.Get("X-Model"))
	mt, params, _ := mime.ParseMediaType(h.Get("Content-Type"))
	trimmed := bytes.TrimSpace(body)
	switch {
	case mt == "multipart/form-data":
		boundary := params["boundary"]
		if boundary == "" {
			return "", nil, errors.New("multipart body without boundary")
		}
		start, end, found, err := findMultipartModel(body, boundary)
		if err != nil {
			return "", nil, err
		}
		if found && header != "" && strings.TrimSpace(string(body[start:end])) == "" {
			return "", nil, errConflict
		}
		if name := strings.TrimSpace(string(body[start:end])); found && name != "" {
			return name, func(m string) ([]byte, error) { return splice(body, start, end, m), nil }, nil
		}
	case len(trimmed) > 0 && (mt == "application/json" || strings.HasSuffix(mt, "+json") || (trimmed[0] == '{' && sniffJSON[mt])):
		start, end, found, err := findJSONModel(body)
		if err != nil {
			return "", nil, err
		}
		if found {
			var name string
			if err := json.Unmarshal(body[start:end], &name); err != nil {
				return "", nil, errors.New(`"model" must be a string`)
			}
			if name != "" {
				return name, func(m string) ([]byte, error) {
					q, err := marshalRaw(m)
					return splice(body, start, end, string(q)), err
				}, nil
			}
		}
	}
	if header == "" {
		return "", nil, errNoModel
	}
	// A body that carries its own (here: unusable) model field must not be
	// silently overridden by the header; parsers may disagree about it.
	if len(trimmed) > 0 && trimmed[0] == '{' {
		if _, _, found, err := findJSONModel(body); found || err == errAmbiguous {
			return "", nil, errConflict
		}
	}
	return header, nil, nil
}

// findJSONModel locates the raw value of the top-level "model" key by
// scanning the object once, without decoding it. The rewrite is then a splice
// (see splice), so every other byte - base64 images, key order, whitespace -
// is forwarded untouched and nothing is re-marshalled. It does not fully
// validate the document; the backend will reject malformed JSON itself.
// Keys equal to "model" case-insensitively (as encoding/json matches) count;
// more than one is rejected rather than guessed at.
func findJSONModel(b []byte) (start, end int, found bool, err error) {
	bad := errMalformed
	i := skipSpace(b, 0)
	if i >= len(b) || b[i] != '{' {
		return 0, 0, false, bad
	}
	i++
	for {
		if i = skipSpace(b, i); i >= len(b) {
			return 0, 0, false, bad
		}
		switch b[i] {
		case '}':
			return start, end, found, nil
		case ',':
			i++
			continue
		case '"':
		default:
			return 0, 0, false, bad
		}
		keyEnd := skipString(b, i)
		if keyEnd < 0 {
			return 0, 0, false, bad
		}
		key := b[i:keyEnd]
		if i = skipSpace(b, keyEnd); i >= len(b) || b[i] != ':' {
			return 0, 0, false, bad
		}
		vs := skipSpace(b, i+1)
		ve := skipValue(b, vs)
		if ve < 0 {
			return 0, 0, false, bad
		}
		if isModelKey(key) {
			if found {
				return 0, 0, false, errAmbiguous // parsers disagree on duplicates: refuse
			}
			start, end, found = vs, ve, true
		}
		i = ve
	}
}

// isModelKey reports whether a quoted JSON key (quotes included) is "model".
func isModelKey(quoted []byte) bool {
	if bytes.IndexByte(quoted, '\\') < 0 {
		return strings.EqualFold(string(quoted), `"model"`)
	}
	var k string
	return json.Unmarshal(quoted, &k) == nil && strings.EqualFold(k, "model")
}

func skipSpace(b []byte, i int) int {
	for i < len(b) && (b[i] == ' ' || b[i] == '\t' || b[i] == '\n' || b[i] == '\r') {
		i++
	}
	return i
}

// skipString returns the index just past the string starting at b[i] == '"',
// or -1. IndexByte keeps multi-megabyte base64 strings cheap.
func skipString(b []byte, i int) int {
	for j := i + 1; ; j++ {
		k := bytes.IndexByte(b[j:], '"')
		if k < 0 {
			return -1
		}
		j += k
		n := 0 // a quote is escaped iff preceded by an odd number of backslashes
		for p := j - 1; p > i && b[p] == '\\'; p-- {
			n++
		}
		if n%2 == 0 {
			return j + 1
		}
	}
}

// skipValue returns the index just past the JSON value starting at b[i], or -1.
func skipValue(b []byte, i int) int {
	if i >= len(b) {
		return -1
	}
	switch b[i] {
	case '"':
		return skipString(b, i)
	case '{', '[':
		depth := 0
		for j := i; j < len(b); j++ {
			switch b[j] {
			case '"':
				if j = skipString(b, j); j < 0 {
					return -1
				}
				j-- // loop increments
			case '{', '[':
				depth++
			case '}', ']':
				if depth--; depth == 0 {
					return j + 1
				}
			}
		}
		return -1
	}
	j := i // number, true, false, null
	for j < len(b) && !strings.ContainsRune(",}] \t\r\n", rune(b[j])) {
		j++
	}
	if j == i {
		return -1
	}
	return j
}

func marshalRaw(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimSuffix(buf.Bytes(), []byte("\n")), nil
}

// splice replaces body[start:end] with model, leaving all other bytes alone.
func splice(body []byte, start, end int, model string) []byte {
	out := make([]byte, 0, len(body)-(end-start)+len(model))
	out = append(out, body[:start]...)
	out = append(out, model...)
	return append(out, body[end:]...)
}

// findMultipartModel locates the value bytes of the form field named "model"
// without copying or re-encoding the rest of the body. mime/multipart cannot
// report offsets, so this walks the parts directly: delimiter line, header
// block, value up to CRLF + delimiter (RFC 2046 §5.1.1).
func findMultipartModel(body []byte, boundary string) (start, end int, found bool, err error) {
	malformed := errors.New("malformed multipart body")
	delim := []byte("--" + boundary)
	next := []byte("\r\n--" + boundary)

	pos := indexDelim(body, delim, true)
	for pos >= 0 {
		pos += len(delim)
		if bytes.HasPrefix(body[pos:], []byte("--")) {
			return start, end, found, nil // closing delimiter
		}
		nl := bytes.IndexByte(body[pos:], '\n') // skip transport padding to end of line
		if nl < 0 {
			return 0, 0, false, malformed
		}
		pos += nl + 1

		var hdr []byte
		valStart := pos
		if bytes.HasPrefix(body[pos:], []byte("\r\n")) { // part with no headers
			valStart = pos + 2
		} else {
			i := bytes.Index(body[pos:], []byte("\r\n\r\n"))
			if i < 0 {
				return 0, 0, false, malformed
			}
			hdr, valStart = body[pos:pos+i], pos+i+4
		}
		i := indexDelim(body[valStart:], next, false)
		if i < 0 {
			return 0, 0, false, malformed
		}
		valEnd := valStart + i
		if partName(hdr) == "model" {
			if found {
				return 0, 0, false, errAmbiguous
			}
			start, end, found = valStart, valEnd, true
		}
		pos = valEnd + 2 // at the next delimiter
	}
	return 0, 0, false, malformed
}

// indexDelim finds delim followed by CRLF, "--", space or tab (RFC 2046), so a
// longer boundary sharing our prefix, or payload bytes that merely look like
// the delimiter, cannot split a part. atLineStart additionally requires it to
// begin a line (or the buffer).
func indexDelim(b, delim []byte, atLineStart bool) int {
	for off := 0; ; {
		i := bytes.Index(b[off:], delim)
		if i < 0 {
			return -1
		}
		i += off
		rest := b[i+len(delim):]
		ok := bytes.HasPrefix(rest, []byte("\r\n")) || bytes.HasPrefix(rest, []byte("--")) ||
			(len(rest) > 0 && (rest[0] == ' ' || rest[0] == '\t'))
		if ok && (!atLineStart || i == 0 || b[i-1] == '\n') {
			return i
		}
		off = i + 1
	}
}

// partName returns the form field name from a part's header block.
func partName(hdr []byte) string {
	r := textproto.NewReader(bufio.NewReader(io.MultiReader(bytes.NewReader(hdr), strings.NewReader("\r\n\r\n"))))
	mh, _ := r.ReadMIMEHeader()
	_, params, _ := mime.ParseMediaType(mh.Get("Content-Disposition"))
	return params["name"]
}
