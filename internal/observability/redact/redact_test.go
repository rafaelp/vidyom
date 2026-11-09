package redact

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestStringRemovesSecretShapes(t *testing.T) {
	cases := []struct{ in, leak string }{
		{"Authorization: Bearer abc.DEF-123_xyz", "abc.DEF-123_xyz"},
		{"auth Basic dXNlcjpwYXNz", "dXNlcjpwYXNz"},
		{"device vdm_01JABCDEF_s3cr3tvalue used", "s3cr3tvalue"},
		{"jwt eyJhbGciOiJIUzI1NiJ9.eyJzdWIiOiIxIn0.c2lnbmF0dXJl end", "eyJzdWIiOiIxIn0"},
		{"key AKIAABCDEFGHIJKLMNOP here", "AKIAABCDEFGHIJKLMNOP"},
		{"key AIzaSyA1234567890abcdefghijklmnopqrstuv here", "AIzaSyA1234567890"},
		{"oauth ya29.a0AfH6SMabcdefghij here", "ya29.a0AfH6SMabcdefghij"},
		{"llm sk-abcdefghijklmnopqrstuvwx here", "sk-abcdefghijklmnopqrstuvwx"},
		{"open https://user:hunter2@media.example.internal/x", "hunter2"},
		{"https://media.example.internal/o?X-Goog-Signature=abcdef&x=1", "abcdef"},
		{"https://media.example.internal/o?a=1&X-Amz-Signature=abcdef", "abcdef"},
		{"https://media.example.internal/o?token=abcdef", "abcdef"},
		{"https://media.example.internal/o?access_token=abcdef#f", "abcdef"},
		{"password=hunter2 next", "hunter2"},
		{`{"token": "abcdef"}`, "abcdef"},
		{"api_key: abcdef", "abcdef"},
		{"-----BEGIN RSA PRIVATE KEY-----\nabcdef\n-----END RSA PRIVATE KEY-----", "abcdef"},
	}
	r := New()
	for _, c := range cases {
		got := r.String(c.in)
		if strings.Contains(got, c.leak) {
			t.Errorf("%q leaked %q: %q", c.in, c.leak, got)
		}
		if !strings.Contains(got, Replacement) {
			t.Errorf("%q has no replacement: %q", c.in, got)
		}
	}
}

func TestStringKeepsOrdinaryText(t *testing.T) {
	cases := []string{
		"job JOB-001 reached node transcode",
		"basic information about the bearer of news",
		"storage key originals/ORI-001/v1/master.mp4",
		"ULID 01HZXYABCDEFGHJKMNPQRSTVWX is the technical key",
		"https://media.example.internal/watch/abc",
		"short",
	}
	r := New()
	for _, c := range cases {
		if got := r.String(c); got != c {
			t.Errorf("changed %q to %q", c, got)
		}
	}
}

func TestAddValueRemovesExactValues(t *testing.T) {
	r := New()
	r.AddValue("plain-looking-value")
	r.AddValue("plain-looking-value") // duplicate is harmless
	r.AddValue("abc")                 // too short to register
	got := r.String("got plain-looking-value and abc")
	if strings.Contains(got, "plain-looking-value") {
		t.Errorf("exact value leaked: %q", got)
	}
	if !strings.Contains(got, "abc") {
		t.Errorf("a short value must not be registered: %q", got)
	}
}

func TestSensitiveKey(t *testing.T) {
	for _, k := range []string{"password", "API_KEY", "api-key", "Authorization", "client_secret", "transcript", "transcript_text", "captions", "signed_url", "sessionToken", "private_key", "Set-Cookie"} {
		if !SensitiveKey(k) {
			t.Errorf("%s should be sensitive", k)
		}
	}
	for _, k := range []string{"credentialsRef", "secret_name", "token_count", "transcript_language", "transcript_id", "transcript_version", "job_id", "message", "duration_ms"} {
		if SensitiveKey(k) {
			t.Errorf("%s should not be sensitive", k)
		}
	}
}

type structWithSecret struct {
	Name     string `json:"name"`
	Password string `json:"password"`
	Nested   struct {
		Note string `json:"note"`
	} `json:"nested"`
}

func TestValueWalksEveryStructure(t *testing.T) {
	r := New()
	s := structWithSecret{Name: "n", Password: "hunter2"}
	s.Nested.Note = "password=hunter3"
	in := map[string]any{
		"token":      "abc123",
		"token_used": 12,
		"note":       "Bearer abc.DEF-123",
		"list":       []any{"password=hunter4", map[string]any{"secret": "x"}},
		"strings":    []string{"password=hunter5"},
		"labels":     map[string]string{"api_key": "abc", "region": "sa"},
		"bytes":      []byte("password=hunter6"),
		"raw":        json.RawMessage(`{"password":"hunter7","ok":1}`),
		"err":        errors.New("password=hunter8"),
		"struct":     s,
		"url":        "https://a:b@media.example.internal",
	}
	out := r.Fields(in)
	data, err := json.Marshal(out)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	for _, leak := range []string{"abc123", "abc.DEF-123", "hunter2", "hunter3", "hunter4", "hunter5", "hunter6", "hunter7", "hunter8", `"x"`, "a:b@"} {
		if strings.Contains(text, leak) {
			t.Errorf("leaked %q in %s", leak, text)
		}
	}
	if !strings.Contains(text, `"region":"sa"`) || !strings.Contains(text, `"token_used":12`) || !strings.Contains(text, `"ok":1`) {
		t.Errorf("ordinary data was lost: %s", text)
	}
}

func TestFieldsNilAndCycle(t *testing.T) {
	r := New()
	if r.Fields(nil) != nil {
		t.Error("nil fields must stay nil")
	}
	cycle := map[string]any{}
	cycle["self"] = cycle
	data, err := json.Marshal(r.Fields(cycle))
	if err != nil {
		t.Fatalf("a cyclic value must not break encoding: %v", err)
	}
	if !strings.Contains(string(data), tooDeep) {
		t.Errorf("expected the depth marker: %s", data)
	}
	if got := r.Value(make(chan int)); got != unserializable {
		t.Errorf("a channel should be unserializable, got %v", got)
	}
}

func TestJSON(t *testing.T) {
	r := New()
	got := string(r.JSON([]byte(`{"password":"x","n":12345678901234567890,"a":["Bearer abc.DEF-123"]}`)))
	if strings.Contains(got, `"x"`) || strings.Contains(got, "abc.DEF-123") {
		t.Errorf("leak: %s", got)
	}
	if !strings.Contains(got, "12345678901234567890") {
		t.Errorf("big number lost: %s", got)
	}
	if got := string(r.JSON([]byte("not json password=abc"))); !json.Valid([]byte(got)) || strings.Contains(got, "=abc") {
		t.Errorf("invalid JSON input must become a redacted string: %s", got)
	}
	if got := r.JSON(nil); got != nil {
		t.Errorf("empty input should stay empty: %q", got)
	}
}

func TestStringTruncatesAtLimit(t *testing.T) {
	r := New()
	got := r.String(strings.Repeat("a ", MaxString))
	if len(got) > MaxString+len(truncated) {
		t.Errorf("length %d exceeds the bound", len(got))
	}
	if !strings.HasSuffix(got, truncated) {
		t.Error("truncation must be marked")
	}
}

func TestErrorAndConcurrency(t *testing.T) {
	r := New()
	if r.Error(nil) != "" {
		t.Error("nil error should be empty")
	}
	if got := r.Error(errors.New("password=hunter2")); strings.Contains(got, "hunter2") {
		t.Errorf("leak: %s", got)
	}
	done := make(chan struct{})
	go func() {
		for range 100 {
			r.AddValue("concurrent-value")
		}
		close(done)
	}()
	for range 100 {
		_ = r.String("text with concurrent-value inside it")
	}
	<-done
}
