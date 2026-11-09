package youtube

import (
	"context"
	"encoding/json"
	"net/http"
	"path/filepath"
	"time"

	"golang.org/x/oauth2"

	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// credentials is the token JSON behind credentialsRef (I230). token_uri, if
// present, is ignored: a secret must not be able to redirect a refresh token.
type credentials struct {
	ClientID     string    `json:"client_id"`
	ClientSecret string    `json:"client_secret"`
	RefreshToken string    `json:"refresh_token"`
	AccessToken  string    `json:"access_token"`
	Expiry       time.Time `json:"expiry"`
}

// open resolves credentials at first use, not at factory time, so a connector can
// open with no vault access and report its public links (I230).
func (c *Connector) open(ctx context.Context) (*http.Client, sessionStore, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.client != nil {
		return c.client, c.sessions, nil
	}
	if c.secrets == nil || c.credentialsRef == "" {
		return nil, nil, errs.New(errs.CodeFailedPrecondition, "this youtube destination has no credentialsRef")
	}
	raw, err := c.secrets.ResolveSecret(ctx, c.credentialsRef)
	if err != nil {
		if errs.IsCode(err, errs.CodeUnavailable) || errs.IsCode(err, errs.CodeSealed) {
			return nil, nil, errs.New(errs.CodeUnavailable, "the vault cannot answer for this destination's credentials")
		}
		return nil, nil, errs.New(errs.CodeFailedPrecondition, "this destination's credentials are not in the vault")
	}
	var cred credentials
	if err := json.Unmarshal(raw, &cred); err != nil {
		// The parse error can quote the secret, so it is dropped.
		return nil, nil, errs.New(errs.CodeFailedPrecondition, "this destination's credentials are not valid token JSON")
	}
	if cred.RefreshToken == "" && cred.AccessToken == "" {
		return nil, nil, errs.New(errs.CodeFailedPrecondition, "this destination's credentials hold neither a refresh token nor an access token")
	}
	if cred.RefreshToken != "" && (cred.ClientID == "" || cred.ClientSecret == "") {
		return nil, nil, errs.New(errs.CodeFailedPrecondition, "a refresh token needs client_id and client_secret")
	}

	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = 5 * time.Minute

	// Token refreshes use their own context: the token source outlives any one job.
	tokenCtx := context.WithValue(context.Background(), oauth2.HTTPClient, &http.Client{
		Transport:     transport,
		Timeout:       apiTimeout,
		CheckRedirect: noRedirects,
	})
	conf := oauth2.Config{
		ClientID:     cred.ClientID,
		ClientSecret: cred.ClientSecret,
		Endpoint:     oauth2.Endpoint{TokenURL: c.oauthURL, AuthStyle: oauth2.AuthStyleInParams},
	}
	source := conf.TokenSource(tokenCtx, &oauth2.Token{
		AccessToken: cred.AccessToken, RefreshToken: cred.RefreshToken, Expiry: cred.Expiry,
	})
	c.client = &http.Client{
		Transport:     &oauth2.Transport{Source: source, Base: transport},
		CheckRedirect: noRedirects,
	}

	if c.sessions == nil {
		if c.stateDir == "" {
			c.sessions = newMemoryStore()
		} else {
			store, err := newFileStore(filepath.Join(c.stateDir, string(c.tenant), c.destination), raw)
			if err != nil {
				c.client = nil
				return nil, nil, err
			}
			c.sessions = store
		}
	} else if fs, ok := c.sessions.(*fileStore); ok {
		// A rotated credential seals new records under its own key. Old ones become
		// unreadable, which reads as "no record", and reconcile then scans (I227).
		fs.rekey(raw)
	}
	return c.client, c.sessions, nil
}

// forgetClient makes the next call resolve the credential again, so one rotated in
// the vault takes effect. Sessions in memory survive this.
func (c *Connector) forgetClient() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.client = nil
}

// noRedirects keeps YouTube's 308 as an answer, and stops a redirect carrying the
// bearer token anywhere: oauth2.Transport would add it to every request it sees.
func noRedirects(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
