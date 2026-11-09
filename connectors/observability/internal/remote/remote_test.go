package remote

import (
	"context"
	"errors"
	"testing"
)

type secretMap map[string]string

func (s secretMap) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	v, ok := s[ref]
	if !ok {
		return nil, errors.New("not found")
	}
	return []byte(v), nil
}

func TestParseEndpoint(t *testing.T) {
	refused := []struct {
		url   string
		creds bool
	}{
		{"", false},
		{"ftp://media.example.internal", false},
		{"https://", false},
		{"https://user:pw@media.example.internal", false},
		{"https://media.example.internal?a=1", false},
		{"https://media.example.internal#f", false},
		{"http://media.example.internal", true},
		{"::not a url", false},
	}
	for _, c := range refused {
		if _, err := ParseEndpoint(c.url, c.creds); err == nil {
			t.Errorf("%q (credentials %v) should be refused", c.url, c.creds)
		}
	}
	accepted := []struct {
		url   string
		creds bool
	}{
		{"http://media.example.internal:3100", false},
		{"https://media.example.internal/loki/", true},
		{"http://127.0.0.1:3100", true},
		{"http://[::1]:3100", true},
		{"http://localhost:3100", true},
	}
	for _, c := range accepted {
		u, err := ParseEndpoint(c.url, c.creds)
		if err != nil {
			t.Errorf("%q (credentials %v): %v", c.url, c.creds, err)
			continue
		}
		if len(u.Path) > 0 && u.Path[len(u.Path)-1] == '/' {
			t.Errorf("%q keeps a trailing slash", c.url)
		}
	}
}

func TestAuthorization(t *testing.T) {
	ctx := context.Background()
	s := secretMap{"basic": "user:pass", "bearer": " abc123\n", "blank": "  "}
	if got, err := Authorization(ctx, s, ""); err != nil || got != "" {
		t.Errorf("no ref: %q %v", got, err)
	}
	if got, err := Authorization(ctx, s, "basic"); err != nil || got != "Basic dXNlcjpwYXNz" {
		t.Errorf("basic: %q %v", got, err)
	}
	if got, err := Authorization(ctx, s, "bearer"); err != nil || got != "Bearer abc123" {
		t.Errorf("bearer: %q %v", got, err)
	}
	for _, ref := range []string{"blank", "missing"} {
		if _, err := Authorization(ctx, s, ref); err == nil {
			t.Errorf("%s should fail", ref)
		}
	}
	if _, err := Authorization(ctx, nil, "basic"); err == nil {
		t.Error("no resolver should fail closed")
	}
}
