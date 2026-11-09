package openai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

type secretStub map[string]string

func (s secretStub) ResolveSecret(_ context.Context, ref string) ([]byte, error) {
	if value, ok := s[ref]; ok {
		return []byte(value), nil
	}
	return nil, errors.New("no such secret")
}

func localEndpoint(baseURL string) config.LLMEndpoint {
	return config.LLMEndpoint{Name: "local-llm", BaseURL: baseURL, Model: "fictional-model", Class: config.EndpointLocal}
}

func chatOK(w http.ResponseWriter, content string) {
	_ = json.NewEncoder(w).Encode(map[string]any{
		"model":   "fictional-model-resolved",
		"choices": []map[string]any{{"message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop"}},
		"usage":   map[string]any{"prompt_tokens": 11, "completion_tokens": 7},
	})
}

func TestCompleteSendsTheOpenAIShape(t *testing.T) {
	var got map[string]any
	var path, auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path, auth = r.URL.Path, r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		chatOK(w, `{"ok":true}`)
	}))
	defer server.Close()

	endpoint := localEndpoint(server.URL + "/v1/")
	endpoint.CredentialsRef = "llm-key"
	llm, err := New(endpoint, secretStub{"llm-key": " fictional-secret\n"}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	response, err := llm.Complete(context.Background(), connector.ChatRequest{
		Messages:    []connector.ChatMessage{{Role: "system", Content: "s"}, {Role: "user", Content: "u"}},
		Temperature: 0.2,
		MaxTokens:   100,
		JSONMode:    true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if path != "/v1/chat/completions" {
		t.Errorf("path = %q", path)
	}
	if auth != "Bearer fictional-secret" {
		t.Errorf("auth = %q", auth)
	}
	if got["model"] != "fictional-model" || got["temperature"] != 0.2 || got["max_tokens"] != float64(100) {
		t.Errorf("request = %v", got)
	}
	if format, _ := got["response_format"].(map[string]any); format["type"] != "json_object" {
		t.Errorf("response_format = %v", got["response_format"])
	}
	if response.Content != `{"ok":true}` || response.Model != "fictional-model-resolved" || response.InputTokens != 11 || response.OutputTokens != 7 {
		t.Errorf("response = %+v", response)
	}
}

func TestNoJSONModeAndNoCredentialByDefault(t *testing.T) {
	var got map[string]any
	var auth string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		auth = r.Header.Get("Authorization")
		_ = json.NewDecoder(r.Body).Decode(&got)
		chatOK(w, "plain")
	}))
	defer server.Close()
	llm, err := New(localEndpoint(server.URL), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}}); err != nil {
		t.Fatal(err)
	}
	if auth != "" {
		t.Errorf("auth = %q", auth)
	}
	if _, present := got["response_format"]; present {
		t.Errorf("response_format sent: %v", got)
	}
}

func TestRetriesTransientFailuresWithTheSameRequest(t *testing.T) {
	for _, status := range []int{http.StatusInternalServerError, http.StatusBadGateway, http.StatusTooManyRequests} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if calls.Add(1) < 3 {
				w.WriteHeader(status)
				return
			}
			chatOK(w, "finally")
		}))
		llm, err := New(localEndpoint(server.URL), nil, Options{Backoff: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		response, err := llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
		server.Close()
		if err != nil {
			t.Fatalf("status %d: %v", status, err)
		}
		if response.Content != "finally" || calls.Load() != 3 {
			t.Fatalf("status %d: %q after %d calls", status, response.Content, calls.Load())
		}
	}
}

func TestGivesUpAfterThreeAttempts(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer server.Close()
	llm, err := New(localEndpoint(server.URL), nil, Options{Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
	if !errs.IsCode(err, errs.CodeUnavailable) || calls.Load() != maxAttempts {
		t.Fatalf("err = %v after %d calls", err, calls.Load())
	}
}

func TestDoesNotRetryClientErrors(t *testing.T) {
	for status, code := range map[int]errs.Code{
		http.StatusBadRequest:   errs.CodeInvalidArgument,
		http.StatusUnauthorized: errs.CodeFailedPrecondition,
		http.StatusForbidden:    errs.CodeFailedPrecondition,
		http.StatusNotFound:     errs.CodeInvalidArgument,
	} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			w.WriteHeader(status)
		}))
		llm, err := New(localEndpoint(server.URL), nil, Options{Backoff: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		_, err = llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
		server.Close()
		if !errs.IsCode(err, code) || calls.Load() != 1 {
			t.Fatalf("status %d: err = %v after %d calls", status, err, calls.Load())
		}
	}
}

func TestErrorsNeverCarryProviderBodiesOrSecrets(t *testing.T) {
	secret := "fictional-secret-value"
	echo := "transcript of a private person"
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		data, _ := io.ReadAll(r.Body)
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write(append([]byte(`{"error":{"message":"invalid: `), data...))
	}))
	defer server.Close()
	endpoint := localEndpoint(server.URL)
	endpoint.CredentialsRef = "k"
	llm, err := New(endpoint, secretStub{"k": secret}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: echo}}})
	if err == nil {
		t.Fatal("expected an error")
	}
	if strings.Contains(err.Error(), echo) || strings.Contains(err.Error(), secret) {
		t.Fatalf("the error leaks content: %v", err)
	}
}

func TestMissingCredentialFailsBeforeSendingAnything(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	endpoint := localEndpoint(server.URL)
	endpoint.CredentialsRef = "absent"
	llm, err := New(endpoint, secretStub{}, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) || calls.Load() != 0 {
		t.Fatalf("err = %v, %d calls", err, calls.Load())
	}
}

// A redirect could carry the transcript to a host the policy never saw (I209).
func TestNeverFollowsRedirects(t *testing.T) {
	var other atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { other.Add(1) }))
	defer target.Close()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Redirect(w, r, target.URL+"/steal", http.StatusTemporaryRedirect)
	}))
	defer server.Close()
	llm, err := New(localEndpoint(server.URL), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	_, err = llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) {
		t.Fatalf("err = %v", err)
	}
	if other.Load() != 0 {
		t.Fatal("the redirect was followed")
	}
}

func TestValidation(t *testing.T) {
	valid := config.LLMEndpoint{Name: "e", BaseURL: "https://llm.example.internal/v1", Model: "m", Class: config.EndpointExternal}
	if _, err := New(valid, nil, Options{}); err != nil {
		t.Fatal(err)
	}
	cases := map[string]func(*config.LLMEndpoint){
		"no model":      func(e *config.LLMEndpoint) { e.Model = "" },
		"no class":      func(e *config.LLMEndpoint) { e.Class = "" },
		"unknown class": func(e *config.LLMEndpoint) { e.Class = "internal" },
		"bad scheme":    func(e *config.LLMEndpoint) { e.BaseURL = "ftp://llm.example.internal" },
		"no host":       func(e *config.LLMEndpoint) { e.BaseURL = "https://" },
		"not a url":     func(e *config.LLMEndpoint) { e.BaseURL = "::" },
		"userinfo":      func(e *config.LLMEndpoint) { e.BaseURL = "https://user:pass@llm.example.internal" },
		"query":         func(e *config.LLMEndpoint) { e.BaseURL = "https://llm.example.internal?key=abc" },
		"external http": func(e *config.LLMEndpoint) { e.BaseURL = "http://llm.example.internal" },
		"needs secrets": func(e *config.LLMEndpoint) { e.CredentialsRef = "k" },
		"credential on lan": func(e *config.LLMEndpoint) {
			e.Class = config.EndpointLocal
			e.BaseURL = "http://llm.example.internal"
			e.CredentialsRef = "k"
		},
	}
	for name, mutate := range cases {
		endpoint := valid
		mutate(&endpoint)
		if _, err := New(endpoint, nil, Options{}); err == nil {
			t.Errorf("%s: must be refused", name)
		}
	}
	loopback := config.LLMEndpoint{Name: "e", BaseURL: "http://127.0.0.1:8000/v1", Model: "m", Class: config.EndpointLocal, CredentialsRef: "k"}
	if _, err := New(loopback, secretStub{}, Options{}); err != nil {
		t.Errorf("a credential over loopback is fine: %v", err)
	}
}

func TestPrivateAddressClassification(t *testing.T) {
	private := []string{"127.0.0.1", "::1", "10.1.2.3", "172.16.0.1", "192.168.1.1", "169.254.1.1", "100.64.0.1", "fd00::1", "fe80::1", "::ffff:10.0.0.1"}
	public := []string{"8.8.8.8", "1.1.1.1", "2001:4860:4860::8888", "100.128.0.1", "172.32.0.1", "::ffff:8.8.8.8", "198.51.100.7"}
	for _, address := range private {
		if !isPrivateAddress(netip.MustParseAddr(address)) {
			t.Errorf("%s must count as private", address)
		}
	}
	for _, address := range public {
		if isPrivateAddress(netip.MustParseAddr(address)) {
			t.Errorf("%s must count as public", address)
		}
	}
}

// A vendor misclassified as local fails closed at dial time, before any byte of
// content leaves (I209). 198.51.100.7 is a documentation address nothing answers on.
func TestLocalEndpointRefusesAPublicAddress(t *testing.T) {
	llm, err := New(localEndpoint("http://198.51.100.7:1/v1"), nil, Options{Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
	if !errs.IsCode(err, errs.CodeFailedPrecondition) || !strings.Contains(err.Error(), "public address") {
		t.Fatalf("err = %v", err)
	}
}

func TestCancellation(t *testing.T) {
	release := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { <-release }))
	defer server.Close()
	defer close(release)
	llm, err := New(localEndpoint(server.URL), nil, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() {
		_, err := llm.Complete(ctx, connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
		done <- err
	}()
	time.Sleep(50 * time.Millisecond)
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("err = %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("did not return")
	}
}

func TestBadResponses(t *testing.T) {
	for name, body := range map[string]string{
		"not json":   "<html>gateway</html>",
		"no choices": `{"choices":[]}`,
	} {
		var calls atomic.Int32
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			calls.Add(1)
			_, _ = w.Write([]byte(body))
		}))
		llm, err := New(localEndpoint(server.URL), nil, Options{Backoff: time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		_, err = llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
		server.Close()
		if !errs.IsCode(err, errs.CodeUnavailable) {
			t.Fatalf("%s: err = %v", name, err)
		}
	}
}

func TestUnreachableIsUnavailable(t *testing.T) {
	server := httptest.NewServer(http.NotFoundHandler())
	url := server.URL
	server.Close()
	llm, err := New(localEndpoint(url), nil, Options{Backoff: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	_, err = llm.Complete(context.Background(), connector.ChatRequest{Messages: []connector.ChatMessage{{Role: "user", Content: "u"}}})
	if !errs.IsCode(err, errs.CodeUnavailable) {
		t.Fatalf("err = %v", err)
	}
}

func TestRegister(t *testing.T) {
	registry := connector.NewRegistry()
	Register(registry)
	factory, err := registry.LLM(Driver)
	if err != nil {
		t.Fatal(err)
	}
	llm, err := factory(context.Background(), config.LLMEndpoint{Name: "e", BaseURL: "http://127.0.0.1:1", Model: "m", Class: config.EndpointLocal}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if llm.Name() != "e" || llm.Class() != config.EndpointLocal || llm.Info().Driver != Driver {
		t.Fatalf("info = %+v", llm.Info())
	}
}
