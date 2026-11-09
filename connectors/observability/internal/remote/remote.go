// Package remote holds the rules shared by connectors that send telemetry over HTTP:
// endpoint validation and credential handling (I276, I278). Configuration carries
// only references, so a URL with credentials in it is refused, and a credential is
// never sent in the clear to anything but this host.
package remote

import (
	"context"
	"encoding/base64"
	"net"
	"net/url"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// ParseEndpoint validates a base URL. withCredentials says a credential will be sent
// to it, which plain HTTP allows only to a loopback address.
func ParseEndpoint(raw string, withCredentials bool) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil {
		return nil, errs.New(errs.CodeValidationFailed, "invalid endpoint URL")
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return nil, errs.New(errs.CodeValidationFailed, "endpoint URL must be http or https")
	}
	if u.Hostname() == "" {
		return nil, errs.New(errs.CodeValidationFailed, "endpoint URL has no host")
	}
	if u.User != nil {
		return nil, errs.New(errs.CodeValidationFailed, "endpoint URL must not hold credentials: use credentialsRef")
	}
	if u.RawQuery != "" || u.Fragment != "" {
		return nil, errs.New(errs.CodeValidationFailed, "endpoint URL must not have a query or fragment")
	}
	if withCredentials && u.Scheme == "http" && !IsLoopback(u.Hostname()) {
		return nil, errs.New(errs.CodeValidationFailed, "credentials over plain HTTP are allowed only to a loopback address")
	}
	u.Path = strings.TrimRight(u.Path, "/")
	return u, nil
}

// IsLoopback reports whether host is a loopback name or address.
func IsLoopback(host string) bool {
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// Authorization resolves a credentialsRef into an Authorization header value. A value
// of the form user:password is Basic, anything else is a Bearer token. An empty ref
// means no header. The value is used at open and kept only in memory.
func Authorization(ctx context.Context, secrets connector.SecretResolver, ref string) (string, error) {
	if ref == "" {
		return "", nil
	}
	if secrets == nil {
		return "", errs.Newf(errs.CodeFailedPrecondition, "credential %q cannot be resolved here", ref)
	}
	value, err := secrets.ResolveSecret(ctx, ref)
	if err != nil {
		return "", errs.Wrap(errs.CodeFailedPrecondition, err, "credential "+ref+" cannot be resolved")
	}
	text := strings.TrimSpace(string(value))
	if text == "" {
		return "", errs.Newf(errs.CodeFailedPrecondition, "credential %q is empty", ref)
	}
	if strings.Contains(text, ":") {
		return "Basic " + base64.StdEncoding.EncodeToString([]byte(text)), nil
	}
	return "Bearer " + text, nil
}
