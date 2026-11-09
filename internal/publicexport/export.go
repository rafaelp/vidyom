// Package publicexport builds a tenant's public.db (D18, I29, R44): the read-only
// dataset the public library reads. Writing it is the only thing here that needs a
// database driver, so this package links sqlitedriver and belongs in vidyomd only.
//
// Everything is rebuilt on each export and replaces the old file atomically. The
// file holds only approved, published, public records, with no storage key, bucket,
// ULID or friendly ID, and no segment marked sensitive (R44, D30). Selection fails
// closed at each step: what cannot be proven releasable is left out. See I231,
// I232 and I233.
package publicexport

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base32"
	"encoding/json"
	"errors"
	"log/slog"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/connector"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/security"
)

// publicIDLength is the length of public_id: 120 bits, lowercase, never the 26
// uppercase characters of a ULID (I233).
const publicIDLength = 24

// Options configures an Exporter.
type Options struct {
	// DataDir is the daemon's data directory; the file lands at
	// <data-dir>/tenants/<tenant>/public.db (§14.1).
	DataDir string
	// Keys derives the tenant's opaque ID key. Required, with no fallback.
	Keys security.DerivedKeys
	// Now defaults to time.Now.
	Now func() time.Time
	// Logger defaults to slog.Default.
	Logger *slog.Logger
}

// Exporter writes public.db files. One tenant's exports run one at a time, so the
// last rename always carries the newest data.
type Exporter struct {
	opts  Options
	locks sync.Map
}

// New returns an Exporter.
func New(opts Options) *Exporter {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	return &Exporter{opts: opts}
}

// Request is one export.
type Request struct {
	Tenant domain.TenantID
	// Store reads the tenant's catalog.
	Store repo.TenantRepos
	// Drivers maps a destination's logical name to its driver name. public.db
	// carries the driver name: the logical name is private configuration.
	Drivers map[string]string
	// Links optionally supplies per-platform links for a public publication.
	Links func(ctx context.Context, p domain.Publication, driver string) connector.PublicLinks
	// Embedder optionally computes vectors when the store cannot return its own.
	Embedder connector.Embedder
}

// EmbeddingSource says where the file's vectors came from (I232).
type EmbeddingSource string

const (
	EmbeddingsNone     EmbeddingSource = "none"
	EmbeddingsStored   EmbeddingSource = "stored"
	EmbeddingsComputed EmbeddingSource = "computed"
)

// Result describes a finished export.
type Result struct {
	Artifact connector.PublicArtifact
	// Records is how many videos the file holds.
	Records int
	// Withheld lists why publications or videos were left out, without content.
	Withheld []Withheld
	// TextWithheld is how many videos lost their transcript, chapters and vectors
	// because their sensitive marks could not be read.
	TextWithheld    int
	EmbeddingModel  string
	EmbeddingSource EmbeddingSource
}

// Withheld is one record the export refused. It is for logs and job output, and
// names only friendly IDs, which stay private to the tenant.
type Withheld struct {
	Asset       id.Friendly `json:"asset"`
	Publication id.Friendly `json:"publication,omitempty"`
	Reason      string      `json:"reason"`
}

// video is one row of videos with everything hanging from it.
type video struct {
	asset       domain.Asset
	publicID    string
	title       string
	description string
	tags        []string
	publishedAt time.Time
	sourceGroup string
	thumbnail   string
	links       []destinationRow
	chapters    []domain.Chapter
	segments    []domain.TranscriptSegment
	marks       []Interval
	vectors     []domain.EmbeddedChunk
}

type destinationRow struct {
	driver     string
	url        string
	embed      string
	timestamps bool
}

// Export rebuilds and atomically replaces the tenant's public.db. A failure leaves
// the previous file in place.
func (e *Exporter) Export(ctx context.Context, req Request) (Result, error) {
	if e.opts.DataDir == "" {
		return Result{}, errs.New(errs.CodeFailedPrecondition, "the public export has no data directory")
	}
	if e.opts.Keys == nil {
		return Result{}, errs.New(errs.CodeFailedPrecondition, "the public export has no key provider")
	}
	if req.Store == nil {
		return Result{}, errs.New(errs.CodeInternal, "the public export has no store")
	}
	if err := req.Tenant.Validate(); err != nil {
		return Result{}, errs.Wrap(errs.CodeInvalidArgument, err, "invalid tenant")
	}
	m, _ := e.locks.LoadOrStore(req.Tenant, &sync.Mutex{})
	mu := m.(*sync.Mutex)
	mu.Lock()
	defer mu.Unlock()

	key, err := e.opts.Keys.PublicIDKey(ctx, req.Tenant)
	if err != nil {
		return Result{}, errs.Wrap(errs.CodeUnavailable, err, "cannot derive the tenant's public ID key")
	}
	if len(key) == 0 {
		return Result{}, errs.New(errs.CodeFailedPrecondition, "the tenant's public ID key is empty")
	}

	b := &builder{req: req, key: key, res: Result{EmbeddingSource: EmbeddingsNone}, log: e.opts.Logger}
	videos, err := b.collect(ctx)
	if err != nil {
		return Result{}, err
	}
	dims, err := b.attachVectors(ctx, videos)
	if err != nil {
		return Result{}, err
	}

	dir := filepath.Join(e.opts.DataDir, "tenants", string(req.Tenant))
	artifact, err := writeFile(ctx, dir, req.Tenant, videos, meta{
		generatedAt:    e.opts.Now().UTC(),
		embeddingModel: b.res.EmbeddingModel,
		dimensions:     dims,
	})
	if err != nil {
		return Result{}, err
	}
	b.res.Artifact = artifact
	b.res.Records = len(videos)
	return b.res, nil
}

type builder struct {
	req Request
	key []byte
	res Result
	log *slog.Logger
}

func (b *builder) withhold(asset, pub id.Friendly, reason string) {
	b.res.Withheld = append(b.res.Withheld, Withheld{Asset: asset, Publication: pub, Reason: reason})
}

// collect selects releasable publications and assembles their videos.
func (b *builder) collect(ctx context.Context) ([]*video, error) {
	store := b.req.Store
	public, err := store.Publications().ListPublic(ctx)
	if err != nil {
		return nil, err
	}
	byAsset := map[id.Friendly][]domain.Publication{}
	for _, p := range public {
		if !p.IsPublic() {
			continue
		}
		ok, err := b.approved(ctx, p)
		if err != nil {
			return nil, err
		}
		if !ok {
			b.withhold(p.AssetID, p.ID, "no approved publication approval")
			continue
		}
		byAsset[p.AssetID] = append(byAsset[p.AssetID], p)
	}

	assetIDs := make([]id.Friendly, 0, len(byAsset))
	for a := range byAsset {
		assetIDs = append(assetIDs, a)
	}
	slices.Sort(assetIDs)

	videos := make([]*video, 0, len(assetIDs))
	for _, assetID := range assetIDs {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		v, err := b.video(ctx, assetID, byAsset[assetID])
		if err != nil {
			return nil, err
		}
		if v != nil {
			videos = append(videos, v)
		}
	}
	return videos, nil
}

// approved reports whether a publication's approval is on record, decided, of a
// kind that authorizes publication, and on the same asset.
func (b *builder) approved(ctx context.Context, p domain.Publication) (bool, error) {
	if p.ApprovalID == "" {
		return false, nil
	}
	a, err := b.req.Store.Approvals().Get(ctx, id.Ref(p.ApprovalID))
	if errs.IsCode(err, errs.CodeNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if a.Status != domain.ApprovalApproved || a.AssetID != p.AssetID {
		return false, nil
	}
	return a.Kind == domain.ApprovalPublication || a.Kind == domain.ApprovalVisibilityChange, nil
}

func (b *builder) video(ctx context.Context, assetID id.Friendly, pubs []domain.Publication) (*video, error) {
	store := b.req.Store
	asset, err := store.Assets().Get(ctx, id.Ref(assetID))
	if errs.IsCode(err, errs.CodeNotFound) {
		b.withhold(assetID, "", "asset not found")
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	switch {
	case asset.DeletedAt != nil:
		b.withhold(assetID, "", "asset deleted")
		return nil, nil
	case asset.Visibility != domain.VisibilityPublic:
		b.withhold(assetID, "", "asset visibility is not public")
		return nil, nil
	case asset.Kind != domain.KindOriginal && asset.Status != domain.AssetPublished:
		b.withhold(assetID, "", "asset status is not published")
		return nil, nil
	}

	sort.SliceStable(pubs, func(i, j int) bool { return publicationBefore(pubs[i], pubs[j]) })
	newest := pubs[len(pubs)-1]

	v := &video{
		asset:       asset,
		publicID:    deriveID(b.key, "public-id", string(asset.ULID)),
		title:       firstNonEmpty(newest.Title, asset.Title),
		description: firstNonEmpty(newest.Description, asset.Description),
		tags:        asset.Tags,
		publishedAt: pubs[0].UpdatedAt,
	}
	if v.title == "" {
		b.withhold(assetID, "", "no title")
		return nil, nil
	}
	if pubs[0].PublishedAt != nil {
		v.publishedAt = *pubs[0].PublishedAt
	}
	if root := rootOf(asset); root != "" {
		v.sourceGroup = deriveID(b.key, "source-group", string(root))
	}
	for _, p := range pubs {
		if row, ok := b.destination(ctx, p, v); ok {
			v.links = append(v.links, row)
		}
	}
	if len(v.links) == 0 {
		b.withhold(assetID, "", "no usable destination URL")
		return nil, nil
	}
	if err := b.attachText(ctx, v, newest.AssetVersion); err != nil {
		return nil, err
	}
	if reason := guardVideo(ctx, store, v); reason != "" {
		b.withhold(assetID, "", reason)
		return nil, nil
	}
	return v, nil
}

// publicationBefore orders by version number, then by date, oldest first.
func publicationBefore(a, b domain.Publication) bool {
	if va, vb := versionNumber(a.AssetVersion), versionNumber(b.AssetVersion); va != vb {
		return va < vb
	}
	return publicationDate(a).Before(publicationDate(b))
}

func publicationDate(p domain.Publication) time.Time {
	if p.PublishedAt != nil {
		return *p.PublishedAt
	}
	return p.UpdatedAt
}

func versionNumber(v string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(v, "v"))
	if err != nil {
		return 0
	}
	return n
}

func rootOf(a domain.Asset) id.Friendly {
	if a.Lineage != nil {
		return firstNonEmpty(a.Lineage.RootAssetID, a.Lineage.SourceAssetID)
	}
	return a.ParentAssetID
}

func firstNonEmpty[T ~string](values ...T) T {
	for _, v := range values {
		if v != "" {
			return v
		}
	}
	return ""
}

func (b *builder) destination(ctx context.Context, p domain.Publication, v *video) (destinationRow, bool) {
	if !httpURL(p.URL) {
		b.withhold(p.AssetID, p.ID, "destination URL is not http(s)")
		return destinationRow{}, false
	}
	driver := b.req.Drivers[p.Destination]
	if driver == "" {
		driver = "other" // never the logical name, which is private configuration
	}
	row := destinationRow{driver: driver, url: p.URL}
	if b.req.Links != nil {
		links := b.req.Links(ctx, p, driver)
		if httpURL(links.EmbedURL) {
			row.embed = links.EmbedURL
		}
		row.timestamps = links.SupportsTimestamps
		if v.thumbnail == "" && httpURL(links.ThumbnailURL) {
			v.thumbnail = links.ThumbnailURL
		}
	}
	return row, true
}

func httpURL(s string) bool {
	if s == "" {
		return false
	}
	u, err := url.Parse(s)
	return err == nil && (u.Scheme == "http" || u.Scheme == "https") && u.Host != ""
}

// attachText adds the transcript and chapters that survive the sensitive filter.
func (b *builder) attachText(ctx context.Context, v *video, version string) error {
	store := b.req.Store
	ref := id.Ref(v.asset.ID)
	marks, err := SensitiveIntervals(ctx, store, v.asset, version)
	if err != nil {
		// Unknown marks mean nothing timed can be proven safe (I231).
		b.res.TextWithheld++
		b.log.Warn("public export withheld a video's text: its sensitive marks could not be read",
			"asset", string(v.asset.ID), "code", string(errs.CodeOf(err)))
		return nil
	}

	v.marks = marks

	transcript, err := store.Transcripts().Get(ctx, ref, version)
	switch {
	case err == nil:
		v.segments = ReleasedSegments(transcript, marks)
	case !errs.IsCode(err, errs.CodeNotFound):
		return err
	}

	analysis, err := store.Transcripts().Analysis(ctx, ref, version)
	switch {
	case err == nil:
		if analysis.Available {
			for _, c := range analysis.Chapters {
				if !Overlaps(marks, c.StartSeconds, c.EndSeconds) {
					v.chapters = append(v.chapters, c)
				}
			}
		}
	case !errs.IsCode(err, errs.CodeNotFound):
		return err
	}

	// Stored vectors are looked up later, per model; keep what filtering needs.
	if lister, ok := store.Search().(repo.EmbeddingLister); ok {
		model, err := store.Search().ActiveModel(ctx)
		if err != nil {
			return err
		}
		if model != "" {
			chunks, err := lister.ListEmbeddings(ctx, ref, version, model)
			if err != nil {
				return err
			}
			for _, c := range chunks {
				if !Overlaps(marks, c.StartSeconds, c.EndSeconds) {
					v.vectors = append(v.vectors, c)
				}
			}
			if len(v.vectors) > 0 {
				b.res.EmbeddingModel = model
				b.res.EmbeddingSource = EmbeddingsStored
			}
		}
	}
	return nil
}

// attachVectors settles on one model for the file and returns its dimensions.
func (b *builder) attachVectors(ctx context.Context, videos []*video) (int, error) {
	if b.res.EmbeddingSource != EmbeddingsStored {
		if _, ok := b.req.Store.Search().(repo.EmbeddingLister); !ok && b.req.Embedder != nil {
			b.computeVectors(ctx, videos)
		}
	}
	dims := 0
	for _, v := range videos {
		for _, c := range v.vectors {
			if dims == 0 {
				dims = len(c.Vector)
			}
			if len(c.Vector) != dims {
				return 0, errs.New(errs.CodeInternal, "stored vectors of one model have different dimensions")
			}
		}
	}
	if dims == 0 {
		b.res.EmbeddingModel = ""
		b.res.EmbeddingSource = EmbeddingsNone
	}
	return dims, nil
}

const embedBatch = 64

// computeVectors embeds released text when the store cannot return its vectors
// (I232). Any failure leaves the whole file with no vectors, and the site then
// reports lexical mode.
func (b *builder) computeVectors(ctx context.Context, videos []*video) {
	if b.req.Embedder.Info().Capabilities["external"] {
		b.log.Warn("public export skipped embedding: the embedder sends content off the host")
		return
	}
	type pending struct {
		v     *video
		chunk domain.SearchChunk
	}
	var work []pending
	for _, v := range videos {
		for _, c := range Windows(v.segments, v.marks) {
			work = append(work, pending{v, c})
		}
		for _, c := range v.chapters {
			work = append(work, pending{v, domain.SearchChunk{
				Level: domain.ChunkChapter, StartSeconds: c.StartSeconds, EndSeconds: c.EndSeconds,
				Text: strings.TrimSpace(c.Title + ". " + c.Summary),
			}})
		}
	}
	model := ""
	out := make([][]float32, len(work))
	for start := 0; start < len(work); start += embedBatch {
		end := min(start+embedBatch, len(work))
		texts := make([]string, 0, end-start)
		for _, w := range work[start:end] {
			texts = append(texts, w.chunk.Text)
		}
		res, err := b.req.Embedder.Embed(ctx, connector.EmbedRequest{Texts: texts, Kind: connector.EmbedPassage})
		if err == nil && len(res.Vectors) != len(texts) {
			err = errors.New("embedder returned a different number of vectors")
		}
		if err == nil && model != "" && res.Model != model {
			err = errors.New("embedder changed model during the export")
		}
		if err != nil {
			b.log.Warn("public export has no vectors: embedding failed", "code", string(errs.CodeOf(err)))
			return
		}
		model = res.Model
		copy(out[start:end], res.Vectors)
	}
	for i, w := range work {
		w.v.vectors = append(w.v.vectors, domain.EmbeddedChunk{SearchChunk: w.chunk, Vector: out[i]})
	}
	if model != "" && len(work) > 0 {
		b.res.EmbeddingModel = model
		b.res.EmbeddingSource = EmbeddingsComputed
	}
}

// deriveID is the opaque ID scheme: HMAC-SHA256 with a label per use, lowercase
// base32 with no padding, truncated (I233).
func deriveID(key []byte, label, value string) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(label))
	mac.Write([]byte{0})
	mac.Write([]byte(value))
	enc := base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(mac.Sum(nil))
	return strings.ToLower(enc[:publicIDLength])
}

// tagsJSON encodes tags as the schema's JSON array, never null.
func tagsJSON(tags []string) (string, error) {
	if tags == nil {
		tags = []string{}
	}
	data, err := json.Marshal(tags)
	return string(data), err
}

// removeStaleTemp clears what a killed export left behind.
func removeStaleTemp(dir string) {
	matches, _ := filepath.Glob(filepath.Join(dir, tempPattern))
	for _, m := range matches {
		_ = os.Remove(m)
	}
}
