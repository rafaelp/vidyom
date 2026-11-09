// Package youtubetest is a local HTTP mock of the YouTube Data API v3, enough for the
// youtube connector and anything exercising it: resumable uploads, videos, thumbnails,
// captions, the channel's uploads playlist and the OAuth token endpoint. It is written
// from the public documentation, so passing against it proves behavior, not that the
// real API agrees (I12, I226). No test calls real YouTube.
//
// It is strict where the documentation is strict, so a mistake in the connector fails
// here: chunks must continue at the received offset and be multiples of 256 KiB,
// `videos.update` resets mutable fields left out and refuses read-only ones, and a
// second caption track for one language is a conflict.
package youtubetest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"slices"
	"strconv"
	"strings"
	"sync"
)

// Op names a kind of request that faults apply to.
type Op string

const (
	OpInitiate Op = "initiate"
	OpChunk    Op = "chunk"  // a chunk that does not carry the last byte
	OpFinal    Op = "final"  // the chunk that carries the last byte
	OpStatus   Op = "status" // a `Content-Range: bytes */N` query
	OpGet      Op = "videos.list"
	OpUpdate   Op = "videos.update"
	OpDelete   Op = "videos.delete"
	OpThumb    Op = "thumbnails.set"
	OpCaptions Op = "captions"
	OpPlaylist Op = "playlistItems.list"
	OpToken    Op = "token"
)

// Fault is what a request gets instead of its normal answer.
type Fault struct {
	// Status answers with this code before doing anything, unless Drop is set.
	Status int
	// Reason goes in the error body, like "quotaExceeded".
	Reason string
	// Drop closes the connection with no answer, before doing anything.
	Drop bool
	// KeepBytes stores this many bytes of a chunk, then drops the connection.
	KeepBytes int
	// DropAfter does the work and then closes the connection, losing the answer.
	DropAfter bool
}

// Video is one video at the mock.
type Video struct {
	ID                  string
	Title               string
	Description         string
	Tags                []string
	CategoryID          string
	DefaultLanguage     string
	Privacy             string
	PublishAt           string
	Embeddable          bool
	License             string
	PublicStatsViewable bool
	MadeForKids         *bool
	UploadStatus        string
	RejectionReason     string
	FailureReason       string
	SHA256              string
	Size                int64
	ThumbnailType       string
	Thumbnail           []byte
	polls               int
}

// Track is one caption track.
type Track struct {
	ID       string
	VideoID  string
	Language string
	Name     string
	Text     string
}

type session struct {
	id       string
	size     int64
	metadata videoJSON
	received bytes.Buffer
	video    *Video
}

// Server is the mock. Fields are read with the Lock methods or after requests end.
type Server struct {
	*httptest.Server

	mu       sync.Mutex
	videos   map[string]*Video
	order    []string // newest first
	tracks   []*Track
	sessions map[string]*session
	faults   map[Op][]Fault
	skip     map[Op]int
	seq      int
	counts   map[Op]int

	clientID     string
	clientSecret string
	refreshToken string
	validTokens  map[string]bool
	tokenSeq     int

	// ProcessingPolls is how many videos.list reads a new video stays "uploaded".
	ProcessingPolls int
	// AfterFinal runs on each new video before the answer, to set a rejection.
	AfterFinal func(*Video)
	// Redirect answers every videos.list with a redirect here, to test it is refused.
	Redirect string
}

// Fictional credentials the mock accepts.
const (
	ClientID     = "fictional-client-id"
	ClientSecret = "fictional-client-secret-value"
	RefreshToken = "fictional-refresh-token-value"
	AccessToken  = "fictional-access-token-value"
)

// New starts a mock. Call Close when done.
func New() *Server {
	s := &Server{
		videos:       map[string]*Video{},
		sessions:     map[string]*session{},
		faults:       map[Op][]Fault{},
		skip:         map[Op]int{},
		counts:       map[Op]int{},
		clientID:     ClientID,
		clientSecret: ClientSecret,
		refreshToken: RefreshToken,
		validTokens:  map[string]bool{AccessToken: true},
	}
	s.Server = httptest.NewServer(http.HandlerFunc(s.serve))
	return s
}

// Parameters returns destination parameters pointing the connector here, with short
// retries, plus any extras.
func (s *Server) Parameters(extra map[string]any) json.RawMessage {
	p := map[string]any{
		"apiBaseUrl":      s.URL,
		"uploadBaseUrl":   s.URL,
		"oauthUrl":        s.URL + "/token",
		"chunkBytes":      256 * 1024,
		"retryWaitMillis": 1,
		"maxRetries":      4,
	}
	for k, v := range extra {
		p[k] = v
	}
	raw, err := json.Marshal(p)
	if err != nil {
		panic(err)
	}
	return raw
}

// Credentials returns token JSON the mock accepts.
func Credentials() []byte {
	raw, err := json.Marshal(map[string]any{
		"client_id": ClientID, "client_secret": ClientSecret, "refresh_token": RefreshToken,
		"access_token": AccessToken, "expiry": "2999-01-01T00:00:00Z",
	})
	if err != nil {
		panic(err)
	}
	return raw
}

// FailNext makes the next calls of op answer with these faults, in order.
func (s *Server) FailNext(op Op, faults ...Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.faults[op] = append(s.faults[op], faults...)
}

// SkipThenFail lets n calls of op through before its faults apply.
func (s *Server) SkipThenFail(op Op, n int, faults ...Fault) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.skip[op] = n
	s.faults[op] = append(s.faults[op], faults...)
}

// Count reports how many calls of op arrived, faulted or not.
func (s *Server) Count(op Op) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.counts[op]
}

// ExpireSessions forgets every open session, as YouTube does after about a week.
func (s *Server) ExpireSessions() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.sessions = map[string]*session{}
}

// RevokeAccessTokens makes every issued access token invalid.
func (s *Server) RevokeAccessTokens() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validTokens = map[string]bool{}
}

// AcceptAccessToken adds a valid access token.
func (s *Server) AcceptAccessToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.validTokens[token] = true
}

// SetRefreshToken changes which refresh token the mock accepts.
func (s *Server) SetRefreshToken(token string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.refreshToken = token
}

// Videos returns copies of all videos, newest first.
func (s *Server) Videos() []Video {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Video, 0, len(s.order))
	for _, id := range s.order {
		out = append(out, *s.videos[id])
	}
	return out
}

// Video returns a copy of one video.
func (s *Server) Video(id string) (Video, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	v, ok := s.videos[id]
	if !ok {
		return Video{}, false
	}
	return *v, true
}

// SetVideo changes a stored video, as something outside Vidyom would.
func (s *Server) SetVideo(id string, change func(*Video)) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if v, ok := s.videos[id]; ok {
		change(v)
	}
}

// AddVideo adds a video that Vidyom did not upload, at the top of the playlist.
func (s *Server) AddVideo(v Video) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.videos[v.ID] = &v
	s.order = append([]string{v.ID}, s.order...)
}

// Tracks returns copies of all caption tracks.
func (s *Server) Tracks() []Track {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]Track, 0, len(s.tracks))
	for _, t := range s.tracks {
		out = append(out, *t)
	}
	return out
}

// OpenSessions reports bytes received per open session.
func (s *Server) OpenSessions() []int {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []int
	for _, sess := range s.sessions {
		if sess.video == nil {
			out = append(out, sess.received.Len())
		}
	}
	slices.Sort(out)
	return out
}

func (s *Server) fault(op Op) *Fault {
	s.counts[op]++
	if s.skip[op] > 0 {
		s.skip[op]--
		return nil
	}
	if len(s.faults[op]) == 0 {
		return nil
	}
	f := s.faults[op][0]
	s.faults[op] = s.faults[op][1:]
	return &f
}

func drop(w http.ResponseWriter) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		panic("youtubetest: cannot hijack")
	}
	conn, _, err := hj.Hijack()
	if err == nil {
		conn.Close()
	}
}

func writeError(w http.ResponseWriter, code int, reason string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(map[string]any{"error": map[string]any{
		"code": code, "message": "mock says " + reason,
		"errors": []map[string]string{{"reason": reason, "domain": "youtube"}},
	}})
}

// applyFault answers for a fault that stops the request. It reports whether it did.
func applyFault(w http.ResponseWriter, f *Fault) bool {
	switch {
	case f == nil:
		return false
	case f.Drop:
		drop(w)
		return true
	case f.Status != 0:
		writeError(w, f.Status, f.Reason)
		return true
	}
	return false
}

func (s *Server) serve(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch {
	case r.URL.Path == "/token":
		s.serveToken(w, r)
	case r.URL.Path == "/upload/youtube/v3/videos" && r.URL.Query().Has("upload_id"):
		s.serveChunk(w, r)
	case r.URL.Path == "/upload/youtube/v3/videos" && r.Method == http.MethodPost:
		s.serveInitiate(w, r)
	case !s.authorized(r):
		writeError(w, http.StatusUnauthorized, "authError")
	case r.URL.Path == "/youtube/v3/videos" && r.Method == http.MethodGet:
		s.serveList(w, r)
	case r.URL.Path == "/youtube/v3/videos" && r.Method == http.MethodPut:
		s.serveUpdate(w, r)
	case r.URL.Path == "/youtube/v3/videos" && r.Method == http.MethodDelete:
		s.serveDelete(w, r)
	case r.URL.Path == "/upload/youtube/v3/thumbnails/set":
		s.serveThumbnail(w, r)
	case r.URL.Path == "/youtube/v3/captions":
		s.serveCaptionList(w, r)
	case r.URL.Path == "/upload/youtube/v3/captions":
		s.serveCaptionUpload(w, r)
	case r.URL.Path == "/youtube/v3/channels":
		writeJSON(w, map[string]any{"items": []any{map[string]any{"contentDetails": map[string]any{"relatedPlaylists": map[string]any{"uploads": "UUfictional"}}}}})
	case r.URL.Path == "/youtube/v3/playlistItems":
		s.servePlaylist(w, r)
	default:
		writeError(w, http.StatusNotFound, "notFound")
	}
}

func (s *Server) authorized(r *http.Request) bool {
	token, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	return ok && s.validTokens[token]
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) serveToken(w http.ResponseWriter, r *http.Request) {
	if applyFault(w, s.fault(OpToken)) {
		return
	}
	if err := r.ParseForm(); err != nil {
		writeError(w, http.StatusBadRequest, "badRequest")
		return
	}
	if r.PostForm.Get("refresh_token") != s.refreshToken || r.PostForm.Get("client_id") != s.clientID || r.PostForm.Get("client_secret") != s.clientSecret {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":"invalid_grant","error_description":"mock refused `+r.PostForm.Get("refresh_token")+`"}`)
		return
	}
	s.tokenSeq++
	token := fmt.Sprintf("mock-access-%d", s.tokenSeq)
	s.validTokens[token] = true
	writeJSON(w, map[string]any{"access_token": token, "token_type": "Bearer", "expires_in": 3600})
}

// JSON shapes of the API.
type snippetJSON struct {
	Title           string   `json:"title"`
	Description     string   `json:"description"`
	Tags            []string `json:"tags,omitempty"`
	CategoryID      string   `json:"categoryId"`
	DefaultLanguage string   `json:"defaultLanguage,omitempty"`
}

type statusJSON struct {
	UploadStatus        string `json:"uploadStatus,omitempty"`
	FailureReason       string `json:"failureReason,omitempty"`
	RejectionReason     string `json:"rejectionReason,omitempty"`
	PrivacyStatus       string `json:"privacyStatus,omitempty"`
	PublishAt           string `json:"publishAt,omitempty"`
	Embeddable          *bool  `json:"embeddable,omitempty"`
	License             string `json:"license,omitempty"`
	PublicStatsViewable *bool  `json:"publicStatsViewable,omitempty"`
	MadeForKids         *bool  `json:"selfDeclaredMadeForKids,omitempty"`
}

type videoJSON struct {
	ID      string       `json:"id,omitempty"`
	Snippet *snippetJSON `json:"snippet,omitempty"`
	Status  *statusJSON  `json:"status,omitempty"`
}

func resource(v *Video, parts string) videoJSON {
	out := videoJSON{ID: v.ID}
	if strings.Contains(parts, "snippet") {
		out.Snippet = &snippetJSON{Title: v.Title, Description: v.Description, Tags: v.Tags, CategoryID: v.CategoryID, DefaultLanguage: v.DefaultLanguage}
	}
	if strings.Contains(parts, "status") {
		embeddable, stats := v.Embeddable, v.PublicStatsViewable
		out.Status = &statusJSON{
			UploadStatus: v.UploadStatus, FailureReason: v.FailureReason, RejectionReason: v.RejectionReason,
			PrivacyStatus: v.Privacy, PublishAt: v.PublishAt, Embeddable: &embeddable, License: v.License,
			PublicStatsViewable: &stats, MadeForKids: v.MadeForKids,
		}
	}
	return out
}

func (s *Server) serveInitiate(w http.ResponseWriter, r *http.Request) {
	if applyFault(w, s.fault(OpInitiate)) {
		return
	}
	if !s.authorized(r) {
		writeError(w, http.StatusUnauthorized, "authError")
		return
	}
	q := r.URL.Query()
	if q.Get("uploadType") != "resumable" || !strings.Contains(q.Get("part"), "snippet") || !strings.Contains(q.Get("part"), "status") {
		writeError(w, http.StatusBadRequest, "badRequest")
		return
	}
	size, err := strconv.ParseInt(r.Header.Get("X-Upload-Content-Length"), 10, 64)
	if err != nil || size <= 0 || r.Header.Get("X-Upload-Content-Type") == "" {
		writeError(w, http.StatusBadRequest, "badContent")
		return
	}
	var meta videoJSON
	if err := json.NewDecoder(r.Body).Decode(&meta); err != nil || meta.Snippet == nil || meta.Snippet.Title == "" || meta.Status == nil {
		writeError(w, http.StatusBadRequest, "invalidVideoMetadata")
		return
	}
	s.seq++
	id := fmt.Sprintf("session%04d", s.seq)
	s.sessions[id] = &session{id: id, size: size, metadata: meta}
	w.Header().Set("Location", s.URL+"/upload/youtube/v3/videos?uploadType=resumable&upload_id="+id)
	w.WriteHeader(http.StatusOK)
}

func setRange(w http.ResponseWriter, received int) {
	if received > 0 {
		w.Header().Set("Range", fmt.Sprintf("bytes=0-%d", received-1))
	}
}

func (s *Server) serveChunk(w http.ResponseWriter, r *http.Request) {
	sess, ok := s.sessions[r.URL.Query().Get("upload_id")]
	contentRange := r.Header.Get("Content-Range")
	spec, ok2 := strings.CutPrefix(contentRange, "bytes ")
	if !ok2 {
		writeError(w, http.StatusBadRequest, "badContentRange")
		return
	}
	span, total, ok3 := strings.Cut(spec, "/")
	if !ok3 {
		writeError(w, http.StatusBadRequest, "badContentRange")
		return
	}

	// A status query.
	if span == "*" {
		if applyFault(w, s.fault(OpStatus)) {
			return
		}
		if !ok {
			writeError(w, http.StatusNotFound, "uploadSessionNotFound")
			return
		}
		if sess.video != nil {
			w.WriteHeader(http.StatusOK)
			_ = json.NewEncoder(w).Encode(resource(sess.video, "snippet,status"))
			return
		}
		setRange(w, sess.received.Len())
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}

	first, last, ok4 := strings.Cut(span, "-")
	start, err1 := strconv.ParseInt(first, 10, 64)
	end, err2 := strconv.ParseInt(last, 10, 64)
	size, err3 := strconv.ParseInt(total, 10, 64)
	if !ok4 || err1 != nil || err2 != nil || err3 != nil || end < start {
		writeError(w, http.StatusBadRequest, "badContentRange")
		return
	}
	op := OpChunk
	if end+1 == size {
		op = OpFinal
	}
	f := s.fault(op)
	if applyFault(w, f) {
		return
	}
	if !ok {
		writeError(w, http.StatusNotFound, "uploadSessionNotFound")
		return
	}
	body, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "badBody")
		return
	}
	if f != nil && f.KeepBytes > 0 {
		if start == int64(sess.received.Len()) {
			sess.received.Write(body[:min(f.KeepBytes, len(body))])
		}
		drop(w)
		return
	}
	switch {
	case size != sess.size:
		writeError(w, http.StatusBadRequest, "badContentRange")
		return
	case start != int64(sess.received.Len()):
		writeError(w, http.StatusBadRequest, "wrongOffset")
		return
	case int64(len(body)) != end-start+1:
		writeError(w, http.StatusBadRequest, "wrongLength")
		return
	case end+1 != size && len(body)%(256*1024) != 0:
		writeError(w, http.StatusBadRequest, "chunkNotMultipleOf256KiB")
		return
	}
	sess.received.Write(body)
	if end+1 != size {
		if f != nil && f.DropAfter {
			drop(w)
			return
		}
		setRange(w, sess.received.Len())
		w.WriteHeader(http.StatusPermanentRedirect)
		return
	}

	sum := sha256.Sum256(sess.received.Bytes())
	s.seq++
	meta := sess.metadata
	v := &Video{
		ID:                  fmt.Sprintf("yt%09d", s.seq),
		Title:               meta.Snippet.Title,
		Description:         meta.Snippet.Description,
		Tags:                meta.Snippet.Tags,
		CategoryID:          meta.Snippet.CategoryID,
		DefaultLanguage:     meta.Snippet.DefaultLanguage,
		Privacy:             meta.Status.PrivacyStatus,
		PublishAt:           meta.Status.PublishAt,
		MadeForKids:         meta.Status.MadeForKids,
		Embeddable:          true,
		License:             "youtube",
		PublicStatsViewable: true,
		UploadStatus:        "uploaded",
		SHA256:              hex.EncodeToString(sum[:]),
		Size:                size,
	}
	if s.AfterFinal != nil {
		s.AfterFinal(v)
	}
	s.videos[v.ID] = v
	s.order = append([]string{v.ID}, s.order...)
	sess.video = v
	if f != nil && f.DropAfter {
		drop(w)
		return
	}
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(resource(v, "snippet,status"))
}

func (s *Server) serveList(w http.ResponseWriter, r *http.Request) {
	if s.Redirect != "" {
		http.Redirect(w, r, s.Redirect, http.StatusFound)
		return
	}
	if applyFault(w, s.fault(OpGet)) {
		return
	}
	q := r.URL.Query()
	items := []videoJSON{}
	for _, id := range strings.Split(q.Get("id"), ",") {
		v, ok := s.videos[id]
		if !ok {
			continue
		}
		if v.UploadStatus == "uploaded" {
			if v.polls >= s.ProcessingPolls {
				v.UploadStatus = "processed"
			}
			v.polls++
		}
		items = append(items, resource(v, q.Get("part")))
	}
	writeJSON(w, map[string]any{"items": items})
}

// serveUpdate applies videos.update's documented rule: a part you send replaces the
// part, so a mutable field left out resets to its default, and read-only fields are
// an error here so the connector is caught sending them.
func (s *Server) serveUpdate(w http.ResponseWriter, r *http.Request) {
	if applyFault(w, s.fault(OpUpdate)) {
		return
	}
	var raw struct {
		ID      string         `json:"id"`
		Snippet map[string]any `json:"snippet"`
		Status  map[string]any `json:"status"`
	}
	if err := json.NewDecoder(r.Body).Decode(&raw); err != nil {
		writeError(w, http.StatusBadRequest, "badRequest")
		return
	}
	v, ok := s.videos[raw.ID]
	if !ok {
		writeError(w, http.StatusNotFound, "videoNotFound")
		return
	}
	parts := r.URL.Query().Get("part")
	if strings.Contains(parts, "status") {
		for _, readOnly := range []string{"uploadStatus", "failureReason", "rejectionReason"} {
			if _, sent := raw.Status[readOnly]; sent {
				writeError(w, http.StatusBadRequest, "readOnlyField")
				return
			}
		}
		if raw.Status["privacyStatus"] == nil {
			writeError(w, http.StatusBadRequest, "invalidVideoMetadata")
			return
		}
		v.Privacy, _ = raw.Status["privacyStatus"].(string)
		v.PublishAt, _ = raw.Status["publishAt"].(string)
		if v.PublishAt != "" && v.Privacy != "private" {
			writeError(w, http.StatusBadRequest, "invalidPublishAt")
			return
		}
		v.Embeddable = boolOr(raw.Status["embeddable"], true)
		v.PublicStatsViewable = boolOr(raw.Status["publicStatsViewable"], false)
		v.License, _ = raw.Status["license"].(string)
		if v.License == "" {
			v.License = "youtube"
		}
	}
	if strings.Contains(parts, "snippet") {
		title, _ := raw.Snippet["title"].(string)
		category, _ := raw.Snippet["categoryId"].(string)
		if title == "" || category == "" {
			writeError(w, http.StatusBadRequest, "invalidVideoMetadata")
			return
		}
		v.Title, v.CategoryID = title, category
		v.Description, _ = raw.Snippet["description"].(string)
		v.DefaultLanguage, _ = raw.Snippet["defaultLanguage"].(string)
		v.Tags = nil
		if tags, ok := raw.Snippet["tags"].([]any); ok {
			for _, t := range tags {
				if text, ok := t.(string); ok {
					v.Tags = append(v.Tags, text)
				}
			}
		}
	}
	writeJSON(w, resource(v, parts))
}

func boolOr(v any, fallback bool) bool {
	if b, ok := v.(bool); ok {
		return b
	}
	return fallback
}

func (s *Server) serveDelete(w http.ResponseWriter, r *http.Request) {
	if applyFault(w, s.fault(OpDelete)) {
		return
	}
	id := r.URL.Query().Get("id")
	if _, ok := s.videos[id]; !ok {
		writeError(w, http.StatusNotFound, "videoNotFound")
		return
	}
	delete(s.videos, id)
	s.order = slices.DeleteFunc(s.order, func(o string) bool { return o == id })
	w.WriteHeader(http.StatusNoContent)
}

func (s *Server) serveThumbnail(w http.ResponseWriter, r *http.Request) {
	if applyFault(w, s.fault(OpThumb)) {
		return
	}
	v, ok := s.videos[r.URL.Query().Get("videoId")]
	if !ok {
		writeError(w, http.StatusNotFound, "videoNotFound")
		return
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		writeError(w, http.StatusBadRequest, "badBody")
		return
	}
	v.Thumbnail, v.ThumbnailType = data, r.Header.Get("Content-Type")
	writeJSON(w, map[string]any{"items": []any{}})
}

func (s *Server) serveCaptionList(w http.ResponseWriter, r *http.Request) {
	if applyFault(w, s.fault(OpCaptions)) {
		return
	}
	items := []any{}
	for _, t := range s.tracks {
		if t.VideoID == r.URL.Query().Get("videoId") {
			items = append(items, map[string]any{"id": t.ID, "snippet": map[string]any{"language": t.Language, "name": t.Name}})
		}
	}
	writeJSON(w, map[string]any{"items": items})
}

func (s *Server) serveCaptionUpload(w http.ResponseWriter, r *http.Request) {
	if applyFault(w, s.fault(OpCaptions)) {
		return
	}
	media, params, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "multipart/related" {
		writeError(w, http.StatusBadRequest, "badContentType")
		return
	}
	reader := multipart.NewReader(r.Body, params["boundary"])
	first, err := reader.NextPart()
	if err != nil {
		writeError(w, http.StatusBadRequest, "missingMetadata")
		return
	}
	var meta struct {
		ID      string `json:"id"`
		Snippet struct {
			VideoID  string `json:"videoId"`
			Language string `json:"language"`
			Name     string `json:"name"`
		} `json:"snippet"`
	}
	if err := json.NewDecoder(first).Decode(&meta); err != nil {
		writeError(w, http.StatusBadRequest, "badMetadata")
		return
	}
	second, err := reader.NextPart()
	if err != nil {
		writeError(w, http.StatusBadRequest, "missingMedia")
		return
	}
	text, err := io.ReadAll(second)
	if err != nil {
		writeError(w, http.StatusBadRequest, "badMedia")
		return
	}
	if r.Method == http.MethodPut {
		for _, t := range s.tracks {
			if t.ID == meta.ID {
				t.Text = string(text)
				writeJSON(w, map[string]any{"id": t.ID})
				return
			}
		}
		writeError(w, http.StatusNotFound, "captionNotFound")
		return
	}
	if _, ok := s.videos[meta.Snippet.VideoID]; !ok {
		writeError(w, http.StatusNotFound, "videoNotFound")
		return
	}
	for _, t := range s.tracks {
		if t.VideoID == meta.Snippet.VideoID && t.Language == meta.Snippet.Language && t.Name == meta.Snippet.Name {
			writeError(w, http.StatusConflict, "captionExists")
			return
		}
	}
	s.seq++
	t := &Track{ID: fmt.Sprintf("track%d", s.seq), VideoID: meta.Snippet.VideoID, Language: meta.Snippet.Language, Name: meta.Snippet.Name, Text: string(text)}
	s.tracks = append(s.tracks, t)
	writeJSON(w, map[string]any{"id": t.ID})
}

func (s *Server) servePlaylist(w http.ResponseWriter, r *http.Request) {
	if applyFault(w, s.fault(OpPlaylist)) {
		return
	}
	q := r.URL.Query()
	max, _ := strconv.Atoi(q.Get("maxResults"))
	if max <= 0 {
		max = 5
	}
	start, _ := strconv.Atoi(q.Get("pageToken"))
	end := min(start+max, len(s.order))
	items := []any{}
	for _, id := range s.order[start:end] {
		items = append(items, map[string]any{"contentDetails": map[string]any{"videoId": id}})
	}
	out := map[string]any{"items": items}
	if end < len(s.order) {
		out["nextPageToken"] = strconv.Itoa(end)
	}
	writeJSON(w, out)
}
