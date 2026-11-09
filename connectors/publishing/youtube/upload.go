package youtube

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/errs"
)

// errSessionGone means YouTube no longer knows the session: it expired or never
// existed. A session that never received its last byte can never have made a video.
var errSessionGone = errors.New("youtube: the upload session is gone")

// Upload sends a video through a resumable session. An idempotency key with a known
// session continues it, and one with a finished session returns that video, so a
// repeat never makes a second one (§15.5). YouTube has no idempotency key of its
// own: the session is what makes this safe.
func (c *Connector) Upload(ctx context.Context, req connector.UploadRequest) (connector.RemoteVideo, error) {
	if req.IdempotencyKey == "" {
		return connector.RemoteVideo{}, errs.New(errs.CodeInvalidArgument, "an upload needs an idempotency key")
	}
	if req.File == nil || req.SizeBytes <= 0 {
		return connector.RemoteVideo{}, errs.New(errs.CodeInvalidArgument, "an upload needs a file with a known size")
	}
	if req.Metadata.Title == "" {
		return connector.RemoteVideo{}, errs.New(errs.CodeInvalidArgument, "youtube requires a title")
	}
	client, store, err := c.open(ctx)
	if err != nil {
		return connector.RemoteVideo{}, err
	}

	sess, err := store.load(req.IdempotencyKey)
	if errors.Is(err, errSealedRecord) {
		sess = nil // sealed under an older credential; reconcile has already scanned
	} else if err != nil {
		return connector.RemoteVideo{}, err
	}
	if sess != nil && sess.VideoID != "" {
		return c.Get(ctx, sess.VideoID)
	}
	if sess != nil && sess.Size != req.SizeBytes {
		sess = nil
	}

	resumed := sess != nil
	restarted := false
	for {
		if sess == nil {
			uri, err := c.initiate(ctx, client, req)
			if errs.IsCode(err, errs.CodeUnauthenticated) {
				// A credential rotated in the vault takes effect here (I230).
				c.forgetClient()
				if client, store, err = c.open(ctx); err == nil {
					uri, err = c.initiate(ctx, client, req)
				}
			}
			if err != nil {
				return connector.RemoteVideo{}, err
			}
			sess = &session{URI: uri, Size: req.SizeBytes, CreatedAt: time.Now().UTC()}
			// Saved before the first byte, so "no record" proves nothing was sent.
			if err := store.save(req.IdempotencyKey, *sess); err != nil {
				return connector.RemoteVideo{}, err
			}
		}
		video, err := c.transfer(ctx, client, sess.URI, req, resumed)
		if errors.Is(err, errSessionGone) && !restarted {
			restarted, resumed, sess = true, false, nil
			continue
		}
		if errors.Is(err, errSessionGone) {
			return connector.RemoteVideo{}, errs.New(errs.CodeUnavailable, "youtube keeps dropping the upload session")
		}
		if err != nil {
			return connector.RemoteVideo{}, err
		}
		sess.VideoID = video.ID
		// The video exists now. Failing to note it is not a failed upload: the node
		// records the ID, and reconcile can find the video by its tag.
		_ = store.save(req.IdempotencyKey, *sess)
		return toRemote(video), nil
	}
}

// initiate opens a session. No video exists until the session is finished.
func (c *Connector) initiate(ctx context.Context, client *http.Client, req connector.UploadRequest) (string, error) {
	st := status{PrivacyStatus: string(req.Metadata.Visibility), SelfDeclaredMadeForKids: c.madeForKids}
	if st.PrivacyStatus == "" {
		st.PrivacyStatus = "private"
	}
	if req.Metadata.ScheduledAt != nil {
		st.PrivacyStatus = "private"
		st.PublishAt = req.Metadata.ScheduledAt.UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(videoResource{
		Snippet: &snippet{
			Title:           cleanTitle(req.Metadata.Title),
			Description:     cleanDescription(req.Metadata.Description),
			Tags:            videoTags(req.IdempotencyKey, req.Metadata.Tags),
			CategoryID:      c.category,
			DefaultLanguage: req.Metadata.Language,
		},
		Status: &st,
	})
	if err != nil {
		return "", fmt.Errorf("encode the upload metadata: %w", err)
	}
	contentType := req.ContentType
	if contentType == "" {
		contentType = "video/*"
	}
	target := c.uploadBase + "/upload/youtube/v3/videos?" + url.Values{"uploadType": {"resumable"}, "part": {"snippet,status"}}.Encode()

	reqCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPost, target, strings.NewReader(string(payload)))
	if err != nil {
		return "", errs.Wrap(errs.CodeInternal, scrubbed(err), "cannot build the youtube request")
	}
	httpReq.Header.Set("Content-Type", "application/json; charset=UTF-8")
	httpReq.Header.Set("X-Upload-Content-Length", strconv.FormatInt(req.SizeBytes, 10))
	httpReq.Header.Set("X-Upload-Content-Type", contentType)
	resp, err := client.Do(httpReq)
	if err != nil {
		return "", transportError(ctx, err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody))
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return "", apiError(resp.StatusCode, data)
	}
	uri := resp.Header.Get("Location")
	if uri == "" {
		return "", errs.New(errs.CodeUnavailable, "youtube opened no upload session")
	}
	if err := checkEndpoint(uri); err != nil {
		return "", errs.New(errs.CodeUnavailable, "youtube named an upload session on an insecure URL")
	}
	return uri, nil
}

// sessionAnswer is what a chunk or a status query got back.
type sessionAnswer struct {
	finished bool
	video    videoResource
	offset   int64 // bytes the session holds, when not finished
	code     int
	body     []byte
}

// transfer sends everything the session lacks, recovering from 5xx and network
// errors by asking the session where it stands. It never restarts on its own.
//
// Classification (D11, I223): a failure while the last byte may have arrived is an
// unknown outcome. Anything earlier is plainly unavailable, because a session cannot
// make a video until it is finished.
func (c *Connector) transfer(ctx context.Context, client *http.Client, uri string, req connector.UploadRequest, resumed bool) (videoResource, error) {
	var offset int64
	failures := 0
	lastByteSent := false

	settle := func(a sessionAnswer) (videoResource, bool, error) {
		switch {
		case a.finished:
			return a.video, true, nil
		case a.code == http.StatusNotFound || a.code == http.StatusGone:
			if lastByteSent {
				return videoResource{}, true, connector.OutcomeUnknown(errs.New(errs.CodeUnavailable, "the upload session vanished after its last byte was sent"))
			}
			return videoResource{}, true, errSessionGone
		case a.code == http.StatusPermanentRedirect:
			if a.offset > offset {
				failures = 0
			}
			offset = a.offset
			lastByteSent = offset >= req.SizeBytes
			if offset > 0 && req.Progress != nil {
				req.Progress(offset)
			}
			return videoResource{}, false, nil
		case a.code >= 500:
			return videoResource{}, false, errRetry
		default:
			return videoResource{}, true, apiError(a.code, a.body)
		}
	}

	recoverNow := resumed
	for {
		if err := ctx.Err(); err != nil {
			return videoResource{}, c.cancelled(err, lastByteSent)
		}

		var answer sessionAnswer
		var err error
		if recoverNow || offset >= req.SizeBytes {
			answer, err = c.queryStatus(ctx, client, uri, req.SizeBytes)
		} else {
			end := min(offset+c.chunk, req.SizeBytes)
			lastByteSent = end == req.SizeBytes
			answer, err = c.putChunk(ctx, client, uri, req, offset, end)
		}

		var done bool
		var video videoResource
		if err == nil {
			video, done, err = settle(answer)
			if done {
				return video, err
			}
		}
		if err == nil {
			// Back on track, unless the session holds everything and has not finished.
			recoverNow = false
			if offset < req.SizeBytes {
				continue
			}
		}
		if ctx.Err() != nil {
			return videoResource{}, c.cancelled(ctx.Err(), lastByteSent)
		}

		failures++
		if failures > c.maxRetries {
			if lastByteSent {
				return videoResource{}, connector.OutcomeUnknown(errs.New(errs.CodeUnavailable, "lost contact with youtube after the upload's last byte"))
			}
			return videoResource{}, errs.New(errs.CodeUnavailable, "lost contact with youtube during the upload; the session can resume")
		}
		recoverNow = true
		select {
		case <-ctx.Done():
			return videoResource{}, c.cancelled(ctx.Err(), lastByteSent)
		case <-time.After(c.retryWait << min(failures-1, 5)):
		}
	}
}

var errRetry = errors.New("youtube: retry")

func (c *Connector) cancelled(err error, lastByteSent bool) error {
	if lastByteSent {
		return connector.OutcomeUnknown(err)
	}
	return err
}

func (c *Connector) putChunk(ctx context.Context, client *http.Client, uri string, req connector.UploadRequest, start, end int64) (sessionAnswer, error) {
	length := end - start
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPut, uri, io.NewSectionReader(req.File, start, length))
	if err != nil {
		return sessionAnswer{}, errs.Wrap(errs.CodeInternal, scrubbed(err), "cannot build the youtube request")
	}
	httpReq.ContentLength = length
	httpReq.GetBody = func() (io.ReadCloser, error) {
		return io.NopCloser(io.NewSectionReader(req.File, start, length)), nil
	}
	httpReq.Header.Set("Content-Range", fmt.Sprintf("bytes %d-%d/%d", start, end-1, req.SizeBytes))
	if req.ContentType != "" {
		httpReq.Header.Set("Content-Type", req.ContentType)
	}
	return c.roundTrip(ctx, client, httpReq)
}

// queryStatus asks a session how much it holds (`Content-Range: bytes */N`).
func (c *Connector) queryStatus(ctx context.Context, client *http.Client, uri string, size int64) (sessionAnswer, error) {
	reqCtx, cancel := context.WithTimeout(ctx, apiTimeout)
	defer cancel()
	httpReq, err := http.NewRequestWithContext(reqCtx, http.MethodPut, uri, http.NoBody)
	if err != nil {
		return sessionAnswer{}, errs.Wrap(errs.CodeInternal, scrubbed(err), "cannot build the youtube request")
	}
	httpReq.ContentLength = 0
	httpReq.Header.Set("Content-Range", fmt.Sprintf("bytes */%d", size))
	return c.roundTrip(ctx, client, httpReq)
}

func (c *Connector) roundTrip(ctx context.Context, client *http.Client, req *http.Request) (sessionAnswer, error) {
	resp, err := client.Do(req)
	if err != nil {
		return sessionAnswer{}, transportError(ctx, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxErrorBody*8))
	if err != nil {
		return sessionAnswer{}, transportError(ctx, err)
	}
	answer := sessionAnswer{code: resp.StatusCode, body: data}
	switch {
	case resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusCreated:
		if err := json.Unmarshal(data, &answer.video); err != nil || answer.video.ID == "" {
			return sessionAnswer{}, connector.OutcomeUnknown(errs.New(errs.CodeUnavailable, "youtube finished the upload with an unreadable answer"))
		}
		answer.finished = true
	case resp.StatusCode == http.StatusPermanentRedirect:
		answer.offset = rangeEnd(resp.Header.Get("Range"))
	}
	return answer, nil
}

// rangeEnd reads `Range: bytes=0-N` as how many bytes the session holds.
func rangeEnd(header string) int64 {
	_, last, ok := strings.Cut(strings.TrimPrefix(header, "bytes="), "-")
	if !ok {
		return 0
	}
	n, err := strconv.ParseInt(last, 10, 64)
	if err != nil || n < 0 {
		return 0
	}
	return n + 1
}

// Reconcile answers whether an earlier upload with this key reached YouTube (I227).
// nil means it has not, and the next Upload continues or starts the session. An
// answer it cannot be sure of is an error, which the node treats as unknown.
func (c *Connector) Reconcile(ctx context.Context, idempotencyKey string) (*connector.RemoteVideo, error) {
	client, store, err := c.open(ctx)
	if err != nil {
		return nil, err
	}
	sess, err := store.load(idempotencyKey)
	sealed := errors.Is(err, errSealedRecord)
	if err != nil && !sealed {
		return nil, err
	}

	if sess != nil {
		if sess.VideoID != "" {
			v, err := c.Get(ctx, sess.VideoID)
			if errs.IsCode(err, errs.CodeNotFound) {
				return &connector.RemoteVideo{RemoteID: sess.VideoID, Status: connector.RemoteRemoved}, nil
			}
			if err != nil {
				return nil, err
			}
			return &v, nil
		}
		answer, err := c.queryStatus(ctx, client, sess.URI, sess.Size)
		if err != nil {
			return nil, err
		}
		switch {
		case answer.finished:
			sess.VideoID = answer.video.ID
			_ = store.save(idempotencyKey, *sess)
			remote := toRemote(answer.video)
			return &remote, nil
		case answer.code == http.StatusPermanentRedirect:
			return nil, nil // still open: no video, and Upload resumes it
		case answer.code == http.StatusNotFound || answer.code == http.StatusGone:
			// Gone with no record of finishing: it may have finished. Look for it.
		case answer.code >= 500:
			return nil, errs.New(errs.CodeUnavailable, "youtube cannot say what happened to the upload session")
		default:
			return nil, apiError(answer.code, answer.body)
		}
	} else if store.durable() && !sealed {
		return nil, nil // a session is saved before its first byte, and there is none
	}
	return c.scan(ctx, idempotencyKey)
}

// scan looks for the reconcile tag in the channel's uploads, newest first. Finding
// nothing with pages left is not an answer, so it is an error (I227).
func (c *Connector) scan(ctx context.Context, idempotencyKey string) (*connector.RemoteVideo, error) {
	var channels struct {
		Items []struct {
			ContentDetails struct {
				RelatedPlaylists struct {
					Uploads string `json:"uploads"`
				} `json:"relatedPlaylists"`
			} `json:"contentDetails"`
		} `json:"items"`
	}
	err := c.call(ctx, http.MethodGet, c.apiBase, "/youtube/v3/channels", url.Values{"part": {"contentDetails"}, "mine": {"true"}}, nil, &channels)
	if err != nil {
		return nil, err
	}
	if len(channels.Items) == 0 || channels.Items[0].ContentDetails.RelatedPlaylists.Uploads == "" {
		return nil, errs.New(errs.CodeUnavailable, "youtube named no uploads playlist for this channel")
	}
	playlist := channels.Items[0].ContentDetails.RelatedPlaylists.Uploads
	tag := tagFor(idempotencyKey)

	token := ""
	scanned := 0
	for {
		var page struct {
			NextPageToken string `json:"nextPageToken"`
			Items         []struct {
				ContentDetails struct {
					VideoID string `json:"videoId"`
				} `json:"contentDetails"`
			} `json:"items"`
		}
		query := url.Values{"part": {"contentDetails"}, "playlistId": {playlist}, "maxResults": {"50"}}
		if token != "" {
			query.Set("pageToken", token)
		}
		if err := c.call(ctx, http.MethodGet, c.apiBase, "/youtube/v3/playlistItems", query, nil, &page); err != nil {
			return nil, err
		}
		ids := make([]string, 0, len(page.Items))
		for _, it := range page.Items {
			ids = append(ids, it.ContentDetails.VideoID)
		}
		if len(ids) > 0 {
			videos, err := c.listVideos(ctx, ids)
			if err != nil {
				return nil, err
			}
			for _, v := range videos {
				if v.Snippet != nil && containsTag(v.Snippet.Tags, tag) {
					remote := toRemote(v)
					return &remote, nil
				}
			}
		}
		scanned += len(ids)
		if page.NextPageToken == "" {
			return nil, nil
		}
		if scanned >= c.scanLimit {
			return nil, errs.Newf(errs.CodeUnavailable, "the channel has more than %d uploads, so this upload cannot be ruled out", c.scanLimit)
		}
		token = page.NextPageToken
	}
}

func containsTag(tags []string, tag string) bool {
	for _, t := range tags {
		if t == tag {
			return true
		}
	}
	return false
}
