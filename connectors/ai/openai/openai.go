package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/config"
	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is the connector's registered name.
const Driver = "openai-compatible"

const (
	connectorVersion = "1"
	// maxResponseBytes bounds a completion read into memory.
	maxResponseBytes = 16 << 20
	requestTimeout   = 10 * time.Minute
	maxAttempts      = 3
)

// Register adds the driver.
func Register(r connector.Registrar) {
	r.RegisterLLM(baseInfo(), Open)
}

func baseInfo() connector.Info {
	return connector.Info{
		Family:       connector.FamilyLLM,
		Driver:       Driver,
		Version:      connectorVersion,
		Available:    true,
		Capabilities: map[string]bool{"json_mode": true},
	}
}

// Open is the driver's factory. It validates the endpoint and fails closed on
// anything it cannot classify.
func Open(_ context.Context, endpoint config.LLMEndpoint, secrets connector.SecretResolver) (connector.LLM, error) {
	return New(endpoint, secrets, Options{})
}

// Options adjusts the driver.
type Options struct {
	// Backoff is the wait before a retry. Tests shorten it. Zero means 1 s, doubling.
	Backoff time.Duration
}

// LLM implements connector.LLM over one endpoint.
type LLM struct {
	endpoint config.LLMEndpoint
	url      string
	secrets  connector.SecretResolver
	client   *http.Client
	backoff  time.Duration
}

var _ connector.LLM = (*LLM)(nil)

// New validates an endpoint and builds its client.
func New(endpoint config.LLMEndpoint, secrets connector.SecretResolver, opts Options) (*LLM, error) {
	if endpoint.Model == "" {
		return nil, errs.Newf(errs.CodeInvalidArgument, "LLM endpoint %q has no model", endpoint.Name)
	}
	if endpoint.Class != config.EndpointLocal && endpoint.Class != config.EndpointExternal {
		return nil, errs.Newf(errs.CodeInvalidArgument, "LLM endpoint %q must be classified local or external", endpoint.Name)
	}
	parsed, err := url.Parse(endpoint.BaseURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return nil, errs.Newf(errs.CodeInvalidArgument, "LLM endpoint %q needs an http or https baseUrl", endpoint.Name)
	}
	if parsed.User != nil || parsed.RawQuery != "" || parsed.Fragment != "" {
		// Credentials go through credentialsRef (§7.1), never inside a URL.
		return nil, errs.Newf(errs.CodeInvalidArgument, "LLM endpoint %q: baseUrl must not carry credentials, a query or a fragment", endpoint.Name)
	}
	if endpoint.Class == config.EndpointExternal && parsed.Scheme != "https" {
		return nil, errs.Newf(errs.CodeInvalidArgument, "external LLM endpoint %q must use https", endpoint.Name)
	}
	if endpoint.CredentialsRef != "" && parsed.Scheme != "https" && !isLoopbackHost(parsed.Hostname()) {
		return nil, errs.Newf(errs.CodeInvalidArgument, "LLM endpoint %q would send credentials over plain http", endpoint.Name)
	}
	if endpoint.CredentialsRef != "" && secrets == nil {
		return nil, errs.Newf(errs.CodeFailedPrecondition, "LLM endpoint %q needs a credential but no secret store is available", endpoint.Name)
	}
	backoff := opts.Backoff
	if backoff == 0 {
		backoff = time.Second
	}
	dialer := &net.Dialer{Timeout: 15 * time.Second}
	if endpoint.Class == config.EndpointLocal {
		dialer.ControlContext = requirePrivateAddress
	}
	transport := &http.Transport{
		DialContext:           dialer.DialContext,
		TLSHandshakeTimeout:   15 * time.Second,
		ResponseHeaderTimeout: requestTimeout,
		MaxIdleConnsPerHost:   2,
		IdleConnTimeout:       90 * time.Second,
		ForceAttemptHTTP2:     true,
		// Never pick up HTTP_PROXY: a proxy would receive the content instead of the
		// classified endpoint.
		Proxy: nil,
	}
	return &LLM{
		endpoint: endpoint,
		url:      strings.TrimRight(endpoint.BaseURL, "/") + "/chat/completions",
		secrets:  secrets,
		backoff:  backoff,
		client: &http.Client{
			Transport: transport,
			Timeout:   requestTimeout,
			// A redirect could carry content to a host the policy never saw.
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}, nil
}

// requirePrivateAddress runs after DNS, on the address about to be dialed, so a
// local-class endpoint cannot reach the public internet through a typo, a
// misclassified vendor or a rebinding attack (I209).
func requirePrivateAddress(_ context.Context, _, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("unparseable address: %w", err)
	}
	ip, err := netip.ParseAddr(host)
	if err != nil {
		return fmt.Errorf("unparseable address: %w", err)
	}
	if !isPrivateAddress(ip) {
		return errPublicAddress
	}
	return nil
}

var errPublicAddress = errors.New("a local-class LLM endpoint resolved to a public address")

var cgnat = netip.MustParsePrefix("100.64.0.0/10")

func isPrivateAddress(ip netip.Addr) bool {
	ip = ip.Unmap()
	return ip.IsLoopback() || ip.IsPrivate() || ip.IsLinkLocalUnicast() || cgnat.Contains(ip)
}

func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}
	ip, err := netip.ParseAddr(host)
	return err == nil && ip.Unmap().IsLoopback()
}

// Info describes the driver for GET /connectors.
func (l *LLM) Info() connector.Info {
	info := baseInfo()
	info.Limits = map[string]any{"model": l.endpoint.Model, "class": string(l.endpoint.Class)}
	return info
}

// Name is the endpoint's configured name.
func (l *LLM) Name() string { return l.endpoint.Name }

// Class is what the external AI policy checks (D8).
func (l *LLM) Class() config.EndpointClass { return l.endpoint.Class }

type wireRequest struct {
	Model          string                  `json:"model"`
	Messages       []connector.ChatMessage `json:"messages"`
	Temperature    float64                 `json:"temperature"`
	MaxTokens      int                     `json:"max_tokens,omitempty"`
	ResponseFormat *wireFormat             `json:"response_format,omitempty"`
}

type wireFormat struct {
	Type string `json:"type"`
}

type wireResponse struct {
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		PromptTokens     int `json:"prompt_tokens"`
		CompletionTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

// Complete sends one chat completion, retrying what is worth retrying (I206). Errors
// say what happened without repeating anything from the provider's response, which
// can echo the prompt.
func (l *LLM) Complete(ctx context.Context, req connector.ChatRequest) (connector.ChatResponse, error) {
	if len(req.Messages) == 0 {
		return connector.ChatResponse{}, errs.New(errs.CodeInvalidArgument, "no messages")
	}
	wire := wireRequest{Model: l.endpoint.Model, Messages: req.Messages, Temperature: req.Temperature, MaxTokens: req.MaxTokens}
	if req.JSONMode {
		wire.ResponseFormat = &wireFormat{Type: "json_object"}
	}
	body, err := json.Marshal(wire)
	if err != nil {
		return connector.ChatResponse{}, errs.New(errs.CodeInvalidArgument, "cannot encode the request")
	}

	wait := l.backoff
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		response, retryAfter, err := l.attempt(ctx, body)
		if err == nil {
			return response, nil
		}
		last = err
		if !retryable(err) || attempt == maxAttempts {
			break
		}
		delay := max(wait, retryAfter)
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return connector.ChatResponse{}, ctx.Err()
		}
		wait *= 2
	}
	return connector.ChatResponse{}, last
}

func retryable(err error) bool {
	switch errs.CodeOf(err) {
	case errs.CodeUnavailable, errs.CodeRateLimited, errs.CodeTimeout:
		return true
	default:
		return false
	}
}

func (l *LLM) attempt(ctx context.Context, body []byte) (connector.ChatResponse, time.Duration, error) {
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, l.url, bytes.NewReader(body))
	if err != nil {
		return connector.ChatResponse{}, 0, errs.Newf(errs.CodeInvalidArgument, "LLM endpoint %q: cannot build the request", l.endpoint.Name)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	if l.endpoint.CredentialsRef != "" {
		secret, err := l.secrets.ResolveSecret(ctx, l.endpoint.CredentialsRef)
		if err != nil {
			return connector.ChatResponse{}, 0, errs.Newf(errs.CodeFailedPrecondition, "LLM endpoint %q: cannot resolve its credential", l.endpoint.Name)
		}
		request.Header.Set("Authorization", "Bearer "+strings.TrimSpace(string(secret)))
	}

	response, err := l.client.Do(request)
	if err != nil {
		return connector.ChatResponse{}, 0, l.transportError(ctx, err)
	}
	defer func() { _ = response.Body.Close() }()
	data, err := io.ReadAll(io.LimitReader(response.Body, maxResponseBytes+1))
	if err != nil {
		return connector.ChatResponse{}, 0, l.transportError(ctx, err)
	}
	if len(data) > maxResponseBytes {
		return connector.ChatResponse{}, 0, errs.Newf(errs.CodeOutputTooLarge, "LLM endpoint %q returned too much data", l.endpoint.Name)
	}

	switch {
	case response.StatusCode >= 200 && response.StatusCode < 300:
	case response.StatusCode == http.StatusTooManyRequests:
		return connector.ChatResponse{}, retryAfter(response), errs.Newf(errs.CodeRateLimited, "LLM endpoint %q is rate limiting", l.endpoint.Name)
	case response.StatusCode >= 500:
		return connector.ChatResponse{}, retryAfter(response), errs.Newf(errs.CodeUnavailable, "LLM endpoint %q answered status %d", l.endpoint.Name, response.StatusCode)
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return connector.ChatResponse{}, 0, errs.Newf(errs.CodeFailedPrecondition, "LLM endpoint %q tried to redirect; redirects are not followed", l.endpoint.Name)
	case response.StatusCode == http.StatusUnauthorized || response.StatusCode == http.StatusForbidden:
		return connector.ChatResponse{}, 0, errs.Newf(errs.CodeFailedPrecondition, "LLM endpoint %q refused the credential (status %d)", l.endpoint.Name, response.StatusCode)
	default:
		return connector.ChatResponse{}, 0, errs.Newf(errs.CodeInvalidArgument, "LLM endpoint %q refused the request (status %d)", l.endpoint.Name, response.StatusCode)
	}

	var decoded wireResponse
	if err := json.Unmarshal(data, &decoded); err != nil {
		return connector.ChatResponse{}, 0, errs.Newf(errs.CodeUnavailable, "LLM endpoint %q returned invalid JSON", l.endpoint.Name)
	}
	if len(decoded.Choices) == 0 {
		return connector.ChatResponse{}, 0, errs.Newf(errs.CodeUnavailable, "LLM endpoint %q returned no choices", l.endpoint.Name)
	}
	model := decoded.Model
	if model == "" {
		model = l.endpoint.Model
	}
	return connector.ChatResponse{
		Content:      decoded.Choices[0].Message.Content,
		Model:        model,
		InputTokens:  decoded.Usage.PromptTokens,
		OutputTokens: decoded.Usage.CompletionTokens,
	}, 0, nil
}

// transportError keeps the cause out of the message, since a URL error can carry the
// request's address, and classifies it.
func (l *LLM) transportError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, errPublicAddress) {
		return errs.Newf(errs.CodeFailedPrecondition, "LLM endpoint %q is classified local but resolved to a public address; refusing to send anything", l.endpoint.Name)
	}
	var timeout interface{ Timeout() bool }
	if errors.As(err, &timeout) && timeout.Timeout() {
		return errs.Newf(errs.CodeTimeout, "LLM endpoint %q timed out", l.endpoint.Name)
	}
	return errs.Newf(errs.CodeUnavailable, "LLM endpoint %q is not reachable", l.endpoint.Name)
}

func retryAfter(response *http.Response) time.Duration {
	seconds, err := strconv.Atoi(response.Header.Get("Retry-After"))
	if err != nil || seconds <= 0 {
		return 0
	}
	return min(time.Duration(seconds)*time.Second, time.Minute)
}
