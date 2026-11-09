// Package youtube is the YouTube publishing connector (§7.3): resumable upload,
// status, thumbnails, captions, visibility and reconcile, over plain REST and
// golang.org/x/oauth2 (I226).
//
// It has never spoken to real YouTube. The wire format follows the public
// documentation and is tested only against the local mock in package youtubetest,
// so it stays Validate (I12, §17.2). The browser OAuth flow does not exist: the
// token JSON is loaded with `vidyom secret add` (I230).
//
// Parameters, all optional:
//
//	{
//	  "stateDir": "/var/lib/vidyom/youtube",    // persist sealed upload sessions (I227)
//	  "categoryId": "22",                       // YouTube requires a category
//	  "chunkBytes": 8388608,                    // rounded to a multiple of 256 KiB
//	  "reconcileScanLimit": 500,                // videos a reconcile scan may read
//	  "selfDeclaredMadeForKids": false,         // left to the channel's setting if absent
//	  "apiBaseUrl": "https://www.googleapis.com",    // for tests
//	  "uploadBaseUrl": "https://www.googleapis.com", // for tests
//	  "oauthUrl": "https://oauth2.googleapis.com/token" // for tests
//	}
package youtube

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"golang.org/x/oauth2"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// Driver is the name used in publishing.json.
const Driver = "youtube"

// Version is the driver version.
const Version = "1.0.0"

const (
	defaultBaseURL  = "https://www.googleapis.com"
	defaultOAuthURL = "https://oauth2.googleapis.com/token"

	defaultCategory  = "22" // People & Blogs; YouTube requires one on update
	chunkUnit        = 256 * 1024
	defaultChunk     = 32 * chunkUnit
	defaultScanLimit = 500
	defaultRetries   = 6
	defaultRetryWait = time.Second

	apiTimeout      = time.Minute
	maxErrorBody    = 1 << 20
	maxThumbnail    = 2 << 20 // YouTube's documented limit
	maxTitleRunes   = 100
	maxDescBytes    = 5000
	maxTagBytes     = 480
	captionTrack    = "Vidyom"
	idempotencyTag  = "vidyom-"
	tagKeyHexLength = 32
)

var safeName = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]*$`)

// Register declares the driver.
func Register(r connector.Registrar) {
	r.RegisterPublishing(info(), open)
}

func info() connector.Info {
	return connector.Info{
		Family:    connector.FamilyPublishing,
		Driver:    Driver,
		Version:   Version,
		Available: true,
		Reason:    "tested only against a local HTTP mock; no browser OAuth flow, load the token JSON with `vidyom secret add` (I12, I230)",
		Capabilities: map[string]bool{
			"resumable_upload": true, "thumbnails": true, "captions": true, "scheduling": true,
			"update": true, "delete": true, "reconcile": true,
		},
	}
}

type parameters struct {
	APIBaseURL              string `json:"apiBaseUrl"`
	UploadBaseURL           string `json:"uploadBaseUrl"`
	OAuthURL                string `json:"oauthUrl"`
	CategoryID              string `json:"categoryId"`
	ChunkBytes              int64  `json:"chunkBytes"`
	StateDir                string `json:"stateDir"`
	ReconcileScanLimit      int    `json:"reconcileScanLimit"`
	SelfDeclaredMadeForKids *bool  `json:"selfDeclaredMadeForKids"`
	MaxRetries              int    `json:"maxRetries"`
	RetryWaitMillis         int    `json:"retryWaitMillis"`
}

func open(_ context.Context, p connector.PublishingParams) (connector.Publishing, error) {
	var params parameters
	if len(p.Config.Parameters) > 0 {
		if err := json.Unmarshal(p.Config.Parameters, &params); err != nil {
			return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid youtube destination parameters")
		}
	}
	if err := p.Tenant.Validate(); err != nil {
		return nil, errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant")
	}
	if !safeName.MatchString(p.Destination) {
		return nil, errs.Newf(errs.CodeInvalidArgument, "invalid destination name %q", p.Destination)
	}
	c := &Connector{
		tenant:         p.Tenant,
		destination:    p.Destination,
		credentialsRef: p.Config.CredentialsRef,
		secrets:        p.Secrets,
		apiBase:        orDefault(params.APIBaseURL, defaultBaseURL),
		uploadBase:     orDefault(params.UploadBaseURL, defaultBaseURL),
		oauthURL:       orDefault(params.OAuthURL, defaultOAuthURL),
		category:       orDefault(params.CategoryID, defaultCategory),
		chunk:          roundChunk(params.ChunkBytes),
		stateDir:       params.StateDir,
		scanLimit:      params.ReconcileScanLimit,
		madeForKids:    params.SelfDeclaredMadeForKids,
		maxRetries:     params.MaxRetries,
		retryWait:      time.Duration(params.RetryWaitMillis) * time.Millisecond,
	}
	if c.scanLimit <= 0 {
		c.scanLimit = defaultScanLimit
	}
	if c.maxRetries <= 0 {
		c.maxRetries = defaultRetries
	}
	if c.retryWait <= 0 {
		c.retryWait = defaultRetryWait
	}
	for _, u := range []string{c.apiBase, c.uploadBase, c.oauthURL} {
		if err := checkEndpoint(u); err != nil {
			return nil, err
		}
	}
	return c, nil
}

func orDefault(v, fallback string) string {
	if v != "" {
		return strings.TrimRight(v, "/")
	}
	return fallback
}

func roundChunk(n int64) int64 {
	if n <= 0 {
		return defaultChunk
	}
	if n < chunkUnit {
		return chunkUnit
	}
	return n - n%chunkUnit
}

// checkEndpoint refuses plaintext except on loopback, because a bearer token goes
// to these URLs (I230).
func checkEndpoint(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" {
		return errs.Newf(errs.CodeInvalidArgument, "invalid youtube endpoint %q", raw)
	}
	switch u.Scheme {
	case "https":
		return nil
	case "http":
		host := u.Hostname()
		if host == "localhost" {
			return nil
		}
		if ip := net.ParseIP(host); ip != nil && ip.IsLoopback() {
			return nil
		}
	}
	return errs.Newf(errs.CodeInvalidArgument, "youtube endpoint %q must be https unless it is on loopback", raw)
}

// Connector is the YouTube driver for one destination.
type Connector struct {
	tenant         domain.TenantID
	destination    string
	credentialsRef string
	secrets        connector.SecretResolver
	apiBase        string
	uploadBase     string
	oauthURL       string
	category       string
	chunk          int64
	stateDir       string
	scanLimit      int
	madeForKids    *bool
	maxRetries     int
	retryWait      time.Duration

	mu       sync.Mutex
	client   *http.Client
	sessions sessionStore
}

var (
	_ connector.Publishing   = (*Connector)(nil)
	_ connector.PublicLinker = (*Connector)(nil)
)

// Info implements connector.Publishing.
func (c *Connector) Info() connector.Info { return info() }

// Capabilities implements connector.Publishing.
func (c *Connector) Capabilities() connector.PublishingCapabilities {
	return connector.PublishingCapabilities{
		ResumableUpload: true,
		Thumbnails:      true,
		Captions:        true,
		Scheduling:      true,
		Update:          true,
		Delete:          true,
		Reconcile:       true,
		Visibilities:    []domain.Visibility{domain.VisibilityPrivate, domain.VisibilityUnlisted, domain.VisibilityPublic},
	}
}

// PublicLinks implements connector.PublicLinker from a confirmed ID, with no call.
// The embed and thumbnail forms are the documented public ones (I233, Validate).
func (c *Connector) PublicLinks(remote connector.RemoteVideo) connector.PublicLinks {
	if remote.RemoteID == "" {
		return connector.PublicLinks{}
	}
	id := url.PathEscape(remote.RemoteID)
	return connector.PublicLinks{
		EmbedURL:           "https://www.youtube.com/embed/" + id,
		ThumbnailURL:       "https://i.ytimg.com/vi/" + id + "/hqdefault.jpg",
		SupportsTimestamps: true,
	}
}

func watchURL(id string) string {
	return "https://www.youtube.com/watch?v=" + url.QueryEscape(id)
}

// ---- Wire types ----

type snippet struct {
	Title           string   `json:"title,omitempty"`
	Description     string   `json:"description,omitempty"`
	Tags            []string `json:"tags,omitempty"`
	CategoryID      string   `json:"categoryId,omitempty"`
	DefaultLanguage string   `json:"defaultLanguage,omitempty"`
}

type status struct {
	UploadStatus            string `json:"uploadStatus,omitempty"`
	FailureReason           string `json:"failureReason,omitempty"`
	RejectionReason         string `json:"rejectionReason,omitempty"`
	PrivacyStatus           string `json:"privacyStatus,omitempty"`
	PublishAt               string `json:"publishAt,omitempty"`
	SelfDeclaredMadeForKids *bool  `json:"selfDeclaredMadeForKids,omitempty"`
}

type videoResource struct {
	ID      string   `json:"id,omitempty"`
	Snippet *snippet `json:"snippet,omitempty"`
	Status  *status  `json:"status,omitempty"`
}

// toRemote maps a video to the contract. The URL is composed only for a video the
// API just confirmed processed: the API has no URL field (R43, D45, I225).
func toRemote(v videoResource) connector.RemoteVideo {
	out := connector.RemoteVideo{RemoteID: v.ID, Status: connector.RemoteProcessing}
	if v.Status == nil {
		return out
	}
	out.Visibility = domain.Visibility(v.Status.PrivacyStatus)
	switch v.Status.UploadStatus {
	case "processed":
		out.Status = connector.RemoteReady
		out.URL = watchURL(v.ID)
		if v.Status.PublishAt != "" {
			out.Status = connector.RemoteScheduled
		}
	case "rejected":
		out.Status = connector.RemoteRejected
		out.Reason = "rejected by YouTube: " + v.Status.RejectionReason
	case "failed":
		out.Status = connector.RemoteFailed
		out.Reason = "failed at YouTube: " + v.Status.FailureReason
	case "deleted":
		out.Status = connector.RemoteRemoved
		out.Reason = "deleted at YouTube"
	}
	return out
}

// tagFor is the marker a reconcile scan looks for. It is an opaque hash prefix
// and says nothing about the tenant or the video (I227).
func tagFor(idempotencyKey string) string {
	key := idempotencyKey
	if len(key) > tagKeyHexLength {
		key = key[:tagKeyHexLength]
	}
	return idempotencyTag + key
}

// videoTags keeps the marker first and drops trailing tags past YouTube's limit.
func videoTags(key string, tags []string) []string {
	out := []string{tagFor(key)}
	used := len(out[0]) + 3
	for _, t := range tags {
		if used+len(t)+3 > maxTagBytes {
			break
		}
		out = append(out, t)
		used += len(t) + 3
	}
	return out
}

func cleanTitle(s string) string {
	s = strings.NewReplacer("<", "", ">", "").Replace(s)
	if utf8.RuneCountInString(s) <= maxTitleRunes {
		return s
	}
	return string([]rune(s)[:maxTitleRunes])
}

func cleanDescription(s string) string {
	s = strings.NewReplacer("<", "", ">", "").Replace(s)
	if len(s) <= maxDescBytes {
		return s
	}
	s = s[:maxDescBytes]
	for !utf8.ValidString(s) {
		s = s[:len(s)-1]
	}
	return s
}

// ---- Errors ----

// scrubbed removes a URL from an error's text. *url.Error prints the URL, and a
// session URI works as a bearer capability, so it must never reach a log (I230).
func scrubbed(err error) error {
	var u *url.Error
	if errors.As(err, &u) {
		return u.Err
	}
	return err
}

var reasonShape = regexp.MustCompile(`^[A-Za-z0-9_.-]{1,64}$`)

// apiError maps an HTTP answer to a coded error, keeping only YouTube's enumerated
// reason and never the body's text.
func apiError(code int, body []byte) error {
	var parsed struct {
		Error struct {
			Errors []struct {
				Reason string `json:"reason"`
			} `json:"errors"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &parsed)
	reason := ""
	if len(parsed.Error.Errors) > 0 && reasonShape.MatchString(parsed.Error.Errors[0].Reason) {
		reason = parsed.Error.Errors[0].Reason
	}
	message := fmt.Sprintf("youtube answered %d", code)
	if reason != "" {
		message += " " + reason
	}
	switch {
	case code == http.StatusUnauthorized:
		return errs.New(errs.CodeUnauthenticated, message)
	case code == http.StatusForbidden:
		switch reason {
		case "quotaExceeded", "dailyLimitExceeded", "rateLimitExceeded", "userRateLimitExceeded", "uploadLimitExceeded":
			return errs.New(errs.CodeRateLimited, message)
		}
		return errs.New(errs.CodeForbidden, message)
	case code == http.StatusNotFound || code == http.StatusGone:
		return errs.New(errs.CodeNotFound, message)
	case code == http.StatusConflict:
		return errs.New(errs.CodeConflict, message)
	case code == http.StatusTooManyRequests:
		return errs.New(errs.CodeRateLimited, message)
	case code >= 500:
		return errs.New(errs.CodeUnavailable, message)
	default:
		return errs.New(errs.CodeInvalidArgument, message)
	}
}

// transportError codes a failed round trip. A refused token is a credentials
// problem; anything else is the network.
func transportError(ctx context.Context, err error) error {
	if ctxErr := ctx.Err(); ctxErr != nil {
		return ctxErr
	}
	var retrieve *oauth2.RetrieveError
	if errors.As(err, &retrieve) {
		// The body of a refused token request stays out of every message.
		return errs.New(errs.CodeUnauthenticated, "the OAuth token endpoint refused this destination's credentials")
	}
	return errs.Wrap(errs.CodeUnavailable, scrubbed(err), "cannot reach youtube")
}

// ---- REST calls ----

// call does one JSON request. On a 401 it drops the cached client and tries once
// more, so a credential rotated in the vault is picked up (I230).
func (c *Connector) call(ctx context.Context, method, base, path string, query url.Values, in, out any) error {
	var payload []byte
	if in != nil {
		var err error
		if payload, err = json.Marshal(in); err != nil {
			return fmt.Errorf("encode youtube request: %w", err)
		}
	}
	for attempt := 0; ; attempt++ {
		err := c.callOnce(ctx, method, base, path, query, payload, out)
		if attempt == 0 && errs.IsCode(err, errs.CodeUnauthenticated) {
			c.forgetClient()
			continue
		}
		return err
	}
}

func (c *Connector) callOnce(ctx context.Context, method, base, path string, query url.Values, payload []byte, out any) error {
	client, _, err := c.open(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	target := base + path
	if len(query) > 0 {
		target += "?" + query.Encode()
	}
	var body io.Reader
	if payload != nil {
		body = bytes.NewReader(payload)
	}
	req, err := http.NewRequestWithContext(ctx, method, target, body)
	if err != nil {
		return errs.Wrap(errs.CodeInternal, scrubbed(err), "cannot build the youtube request")
	}
	if payload != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return transportError(ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody*8))
	if err != nil {
		return transportError(ctx, err)
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp.StatusCode, data)
	}
	if out != nil && len(data) > 0 {
		if err := json.Unmarshal(data, out); err != nil {
			return errs.Wrap(errs.CodeUnavailable, err, "youtube answered with something unreadable")
		}
	}
	return nil
}

func (c *Connector) listVideos(ctx context.Context, ids []string) ([]videoResource, error) {
	var out struct {
		Items []videoResource `json:"items"`
	}
	err := c.call(ctx, http.MethodGet, c.apiBase, "/youtube/v3/videos", url.Values{
		"part": {"snippet,status"},
		"id":   {strings.Join(ids, ",")},
	}, nil, &out)
	return out.Items, err
}

func (c *Connector) getVideo(ctx context.Context, remoteID string) (videoResource, error) {
	items, err := c.listVideos(ctx, []string{remoteID})
	if err != nil {
		return videoResource{}, err
	}
	if len(items) == 0 {
		return videoResource{}, errs.NotFound("video")
	}
	return items[0], nil
}

// Get implements connector.Publishing. A video YouTube no longer lists is not_found.
func (c *Connector) Get(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
	v, err := c.getVideo(ctx, remoteID)
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	return toRemote(v), nil
}

// GetStatus implements connector.Publishing.
func (c *Connector) GetStatus(ctx context.Context, remoteID string) (connector.RemoteVideo, error) {
	return c.Get(ctx, remoteID)
}

// GetURL returns a URL only for a video confirmed processed (R43).
func (c *Connector) GetURL(ctx context.Context, remoteID string) (string, error) {
	v, err := c.Get(ctx, remoteID)
	if err != nil {
		return "", err
	}
	if v.URL == "" {
		return "", errs.New(errs.CodeFailedPrecondition, "this video has no URL yet")
	}
	return v.URL, nil
}

// raw is a resource kept as JSON for read-modify-write: videos.update resets any
// mutable field left out of a part it receives.
type rawVideo struct {
	ID      string         `json:"id"`
	Snippet map[string]any `json:"snippet,omitempty"`
	Status  map[string]any `json:"status,omitempty"`
}

// Writable fields, per the documentation. Read-only ones must not be sent back.
var (
	writableSnippet = []string{"title", "description", "tags", "categoryId", "defaultLanguage", "defaultAudioLanguage"}
	writableStatus  = []string{"privacyStatus", "publishAt", "embeddable", "license", "publicStatsViewable", "selfDeclaredMadeForKids", "containsSyntheticMedia"}
)

func keep(m map[string]any, fields []string) map[string]any {
	out := map[string]any{}
	for _, f := range fields {
		if v, ok := m[f]; ok {
			out[f] = v
		}
	}
	return out
}

func (c *Connector) readForWrite(ctx context.Context, remoteID, parts string) (rawVideo, error) {
	var out struct {
		Items []rawVideo `json:"items"`
	}
	err := c.call(ctx, http.MethodGet, c.apiBase, "/youtube/v3/videos", url.Values{"part": {parts}, "id": {remoteID}}, nil, &out)
	if err != nil {
		return rawVideo{}, err
	}
	if len(out.Items) == 0 {
		return rawVideo{}, errs.NotFound("video")
	}
	return out.Items[0], nil
}

// Update implements connector.Publishing. It keeps the reconcile marker, and a
// schedule makes the video private with publishAt, as YouTube requires.
func (c *Connector) Update(ctx context.Context, remoteID string, meta connector.VideoMetadata) (connector.RemoteVideo, error) {
	current, err := c.readForWrite(ctx, remoteID, "snippet,status")
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	sn := keep(current.Snippet, writableSnippet)
	st := keep(current.Status, writableStatus)
	if meta.Title != "" {
		sn["title"] = cleanTitle(meta.Title)
	}
	sn["description"] = cleanDescription(meta.Description)
	if meta.Language != "" {
		sn["defaultLanguage"] = meta.Language
	}
	if _, ok := sn["categoryId"]; !ok {
		sn["categoryId"] = c.category
	}
	if meta.Tags != nil {
		tags := append([]string{}, existingMarker(current.Snippet)...)
		sn["tags"] = append(tags, meta.Tags...)
	}
	if meta.Visibility != "" {
		st["privacyStatus"] = string(meta.Visibility)
	}
	if meta.ScheduledAt != nil {
		st["privacyStatus"] = string(domain.VisibilityPrivate)
		st["publishAt"] = meta.ScheduledAt.UTC().Format(time.RFC3339)
	} else {
		delete(st, "publishAt")
	}
	var out videoResource
	err = c.call(ctx, http.MethodPut, c.apiBase, "/youtube/v3/videos", url.Values{"part": {"snippet,status"}},
		rawVideo{ID: remoteID, Snippet: sn, Status: st}, &out)
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	return toRemote(out), nil
}

func existingMarker(sn map[string]any) []string {
	raw, _ := sn["tags"].([]any)
	for _, t := range raw {
		if s, ok := t.(string); ok && strings.HasPrefix(s, idempotencyTag) {
			return []string{s}
		}
	}
	return nil
}

// SetVisibility implements connector.Publishing with read-modify-write on status.
func (c *Connector) SetVisibility(ctx context.Context, remoteID string, v domain.Visibility) (connector.RemoteVideo, error) {
	current, err := c.readForWrite(ctx, remoteID, "status")
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	st := keep(current.Status, writableStatus)
	st["privacyStatus"] = string(v)
	delete(st, "publishAt") // only valid on a private video, and this decides it now
	var out videoResource
	err = c.call(ctx, http.MethodPut, c.apiBase, "/youtube/v3/videos", url.Values{"part": {"status"}},
		rawVideo{ID: remoteID, Status: st}, &out)
	if err != nil {
		return connector.RemoteVideo{}, err
	}
	return toRemote(out), nil
}

// Delete removes a video. A video already gone is success: withdrawal is repeatable.
func (c *Connector) Delete(ctx context.Context, remoteID string) error {
	err := c.call(ctx, http.MethodDelete, c.apiBase, "/youtube/v3/videos", url.Values{"id": {remoteID}}, nil, nil)
	if errs.IsCode(err, errs.CodeNotFound) {
		return nil
	}
	return err
}

// send posts a media body and reads the answer, for thumbnails and captions.
func (c *Connector) send(ctx context.Context, method, target, contentType string, body []byte) error {
	for attempt := 0; ; attempt++ {
		err := c.sendOnce(ctx, method, target, contentType, body)
		if attempt == 0 && errs.IsCode(err, errs.CodeUnauthenticated) {
			c.forgetClient()
			continue
		}
		return err
	}
}

func (c *Connector) sendOnce(ctx context.Context, method, target, contentType string, body []byte) error {
	client, _, err := c.open(ctx)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, target, bytes.NewReader(body))
	if err != nil {
		return errs.Wrap(errs.CodeInternal, scrubbed(err), "cannot build the youtube request")
	}
	req.Header.Set("Content-Type", contentType)
	resp, err := client.Do(req)
	if err != nil {
		return transportError(ctx, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return apiError(resp.StatusCode, data)
	}
	return nil
}

// UploadThumbnail implements connector.Publishing.
func (c *Connector) UploadThumbnail(ctx context.Context, remoteID string, image io.Reader, contentType string) error {
	data, err := io.ReadAll(io.LimitReader(image, maxThumbnail+1))
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot read the thumbnail")
	}
	if len(data) > maxThumbnail {
		return errs.New(errs.CodeInvalidArgument, "a youtube thumbnail may not exceed 2 MiB")
	}
	if contentType == "" {
		contentType = "image/jpeg"
	}
	target := c.uploadBase + "/upload/youtube/v3/thumbnails/set?" + url.Values{"videoId": {remoteID}, "uploadType": {"media"}}.Encode()
	return c.send(ctx, http.MethodPost, target, contentType, data)
}

// UploadCaptions implements connector.Publishing. It replaces this connector's track
// for the language rather than adding another, so a retry leaves one track.
func (c *Connector) UploadCaptions(ctx context.Context, remoteID, language, _ string, captions io.Reader) error {
	data, err := io.ReadAll(captions)
	if err != nil {
		return errs.Wrap(errs.CodeUnavailable, err, "cannot read the captions")
	}
	var tracks struct {
		Items []struct {
			ID      string `json:"id"`
			Snippet struct {
				Language string `json:"language"`
				Name     string `json:"name"`
			} `json:"snippet"`
		} `json:"items"`
	}
	err = c.call(ctx, http.MethodGet, c.apiBase, "/youtube/v3/captions", url.Values{"part": {"snippet"}, "videoId": {remoteID}}, nil, &tracks)
	if err != nil {
		return err
	}
	metadata := map[string]any{"snippet": map[string]any{
		"videoId": remoteID, "language": language, "name": captionTrack, "isDraft": false,
	}}
	method := http.MethodPost
	for _, t := range tracks.Items {
		if t.Snippet.Language == language && t.Snippet.Name == captionTrack {
			method = http.MethodPut
			metadata = map[string]any{"id": t.ID, "snippet": map[string]any{"isDraft": false}}
			break
		}
	}
	body, contentType, err := multipartRelated(metadata, data)
	if err != nil {
		return err
	}
	target := c.uploadBase + "/upload/youtube/v3/captions?" + url.Values{"part": {"snippet"}, "uploadType": {"multipart"}}.Encode()
	return c.send(ctx, method, target, contentType, body)
}

// multipartRelated builds the body of a metadata-plus-media upload.
func multipartRelated(metadata any, media []byte) ([]byte, string, error) {
	meta, err := json.Marshal(metadata)
	if err != nil {
		return nil, "", fmt.Errorf("encode caption metadata: %w", err)
	}
	const boundary = "vidyom-caption-boundary"
	var b bytes.Buffer
	fmt.Fprintf(&b, "--%s\r\nContent-Type: application/json; charset=UTF-8\r\n\r\n", boundary)
	b.Write(meta)
	fmt.Fprintf(&b, "\r\n--%s\r\nContent-Type: application/octet-stream\r\n\r\n", boundary)
	b.Write(media)
	fmt.Fprintf(&b, "\r\n--%s--\r\n", boundary)
	return b.Bytes(), "multipart/related; boundary=" + boundary, nil
}
