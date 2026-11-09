// Package redact removes secrets and private content from anything about to leave
// the process as telemetry (§10.1, §9.4, I274). It is a second layer: callers must
// not log secrets in the first place, and no pattern list can catch every secret.
package redact

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"
)

// Replacement stands in for removed content.
const Replacement = "[REDACTED]"

// MaxString bounds any single string that passes through.
const MaxString = 64 << 10

const (
	truncated      = "...[TRUNCATED]"
	unserializable = "[UNSERIALIZABLE]"
	tooDeep        = "[TOO DEEP]"
	maxDepth       = 32
	minValueLength = 4
)

// sensitiveKeyParts mark a field whose value must not be logged. Keys are compared
// lowercased with separators removed.
var sensitiveKeyParts = []string{
	"secret", "token", "password", "passwd", "apikey", "privatekey", "credential",
	"authorization", "cookie", "signedurl", "signature", "transcript", "captions",
}

// harmlessKeySuffixes name metadata about a sensitive thing, not the thing:
// credentialsRef, secret_name, token_count, transcript_language.
var harmlessKeySuffixes = []string{
	"ref", "id", "name", "count", "version", "language", "status", "model", "uri", "available",
}

// SensitiveKey reports whether a field of this name must have its value removed.
func SensitiveKey(key string) bool {
	k := strings.Map(func(r rune) rune {
		switch r {
		case '_', '-', ' ', '.':
			return -1
		}
		return r
	}, strings.ToLower(key))
	found := false
	for _, part := range sensitiveKeyParts {
		if strings.Contains(k, part) {
			found = true
			break
		}
	}
	if !found {
		return false
	}
	for _, suffix := range harmlessKeySuffixes {
		if strings.HasSuffix(k, suffix) {
			return false
		}
	}
	return true
}

var (
	rePEM = regexp.MustCompile(`(?s)-----BEGIN [A-Z0-9 ]*PRIVATE KEY-----.*?-----END [A-Z0-9 ]*PRIVATE KEY-----`)
	// Scheme followed by a token. looksLikeCredential decides, so that "basic
	// information" and "bearer of news" stay as they are.
	reAuthScheme = regexp.MustCompile(`(?i)\b(bearer|basic)(\s+)([A-Za-z0-9._~+/=-]{6,})`)
	reDevice     = regexp.MustCompile(`\bvdm_[A-Za-z0-9]+_[A-Za-z0-9_-]+`)
	reJWT        = regexp.MustCompile(`\beyJ[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]{5,}\.[A-Za-z0-9_-]*`)
	reCloud      = regexp.MustCompile(`\b(?:AKIA[0-9A-Z]{16}|AIza[0-9A-Za-z_-]{35}|ya29\.[0-9A-Za-z_-]{10,}|sk-[A-Za-z0-9_-]{16,}|gh[pousr]_[A-Za-z0-9]{20,}|xox[abprs]-[A-Za-z0-9-]{10,})\b`)
	reUserInfo   = regexp.MustCompile(`([a-zA-Z][a-zA-Z0-9+.-]*://)[^/\s:@'"]+:[^/\s@'"]+@`)
	reQuery      = regexp.MustCompile(`(?i)([?&](?:x-goog-signature|x-goog-credential|x-amz-signature|x-amz-credential|x-amz-security-token|sig|signature|token|access_token|refresh_token|id_token|api_key|apikey|key|password|secret)=)[^&\s"'#]+`)
	reAssign     = regexp.MustCompile(`(?i)\b(password|passwd|secret|token|api[_-]?key|access[_-]?key|private[_-]?key|authorization)("?\s*[=:]\s*)("[^"]*"|'[^']*'|[^\s,;&"'}\])]+)`)
)

func looksLikeCredential(tok string) bool {
	if len(tok) >= 20 {
		return true
	}
	for _, r := range tok {
		switch {
		case r >= 'a' && r <= 'z':
		default:
			return true
		}
	}
	return false
}

// Redactor applies the rules. Exact values added with AddValue are removed too.
// It is safe for concurrent use. The zero value is usable.
type Redactor struct {
	mu     sync.RWMutex
	values []string
}

// New returns a Redactor.
func New() *Redactor { return &Redactor{} }

// AddValue registers an exact secret value. Very short values are ignored, since
// removing them would destroy unrelated text.
func (r *Redactor) AddValue(value string) {
	if len(value) < minValueLength {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	for _, v := range r.values {
		if v == value {
			return
		}
	}
	r.values = append(r.values, value)
	sort.Slice(r.values, func(a, b int) bool { return len(r.values[a]) > len(r.values[b]) })
}

// String redacts free text and bounds its length.
func (r *Redactor) String(s string) string {
	if s == "" {
		return s
	}
	r.mu.RLock()
	for _, v := range r.values {
		if strings.Contains(s, v) {
			s = strings.ReplaceAll(s, v, Replacement)
		}
	}
	r.mu.RUnlock()
	if len(s) > MaxString {
		s = s[:MaxString] + truncated
	}
	if len(s) < 8 {
		return s
	}
	s = rePEM.ReplaceAllString(s, Replacement)
	s = reAuthScheme.ReplaceAllStringFunc(s, func(m string) string {
		parts := reAuthScheme.FindStringSubmatch(m)
		if !looksLikeCredential(parts[3]) {
			return m
		}
		return parts[1] + parts[2] + Replacement
	})
	s = reDevice.ReplaceAllString(s, Replacement)
	s = reJWT.ReplaceAllString(s, Replacement)
	s = reCloud.ReplaceAllString(s, Replacement)
	s = reUserInfo.ReplaceAllString(s, "${1}"+Replacement+"@")
	s = reQuery.ReplaceAllString(s, "${1}"+Replacement)
	s = reAssign.ReplaceAllString(s, "${1}${2}"+Replacement)
	return s
}

// Error redacts an error's text. A nil error is empty.
func (r *Redactor) Error(err error) string {
	if err == nil {
		return ""
	}
	return r.String(err.Error())
}

// Value redacts any value. Structured values are walked by key and value; a type
// it does not know goes through JSON, so a struct field cannot bypass the walk.
func (r *Redactor) Value(v any) any { return r.value(v, 0) }

// Fields redacts a field map, returning a new map. A nil map stays nil.
func (r *Redactor) Fields(m map[string]any) map[string]any {
	if m == nil {
		return nil
	}
	return r.walkMap(m, 0)
}

// JSON redacts a JSON document, returning a valid document. Input that is not JSON
// is treated as text and returned as a JSON string.
func (r *Redactor) JSON(raw []byte) []byte {
	if len(raw) == 0 {
		return raw
	}
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		out, _ := json.Marshal(r.String(string(raw)))
		return out
	}
	out, err := json.Marshal(r.value(doc, 0))
	if err != nil {
		return []byte(`"` + unserializable + `"`)
	}
	return out
}

func (r *Redactor) value(v any, depth int) any {
	if depth > maxDepth {
		return tooDeep
	}
	switch x := v.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number, time.Time, time.Duration:
		return x
	case string:
		return r.String(x)
	case []byte:
		return r.String(string(x))
	case json.RawMessage:
		var doc any
		dec := json.NewDecoder(bytes.NewReader(x))
		dec.UseNumber()
		if err := dec.Decode(&doc); err != nil {
			return r.String(string(x))
		}
		return r.value(doc, depth+1)
	case error:
		return r.String(x.Error())
	case map[string]any:
		return r.walkMap(x, depth)
	case map[string]string:
		out := make(map[string]string, len(x))
		for k, val := range x {
			if SensitiveKey(k) {
				out[k] = Replacement
			} else {
				out[k] = r.String(val)
			}
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = r.value(item, depth+1)
		}
		return out
	case []string:
		out := make([]string, len(x))
		for i, item := range x {
			out[i] = r.String(item)
		}
		return out
	case fmt.Stringer:
		return r.String(x.String())
	}
	data, err := json.Marshal(v)
	if err != nil {
		return unserializable
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return unserializable
	}
	return r.value(doc, depth+1)
}

func (r *Redactor) walkMap(m map[string]any, depth int) map[string]any {
	out := make(map[string]any, len(m))
	for k, val := range m {
		if SensitiveKey(k) && !isScalarNonText(val) {
			out[k] = Replacement
			continue
		}
		out[k] = r.value(val, depth+1)
	}
	return out
}

// isScalarNonText reports values that cannot hold a secret: token_total: 12.
func isScalarNonText(v any) bool {
	switch v.(type) {
	case nil, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, json.Number:
		return true
	}
	return false
}
