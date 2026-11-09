package catalog

import (
	"context"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/api"
	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
	"github.com/rafaelp/vidyom/internal/contract/service"
)

// maxLineageDepth bounds a walk along a lineage; real chains are short (D36).
const maxLineageDepth = 16

// effectiveMarks is what must stay hidden from a reader of this asset version: its own
// marks plus each lineage source's, mapped onto its timeline, so a mark added to a source
// after a clip was cut still counts (D30, I207, I231). The clip's timeline is its lineage
// segments in order. A version other than the current one may have been cut from other
// segments, so a source mark on it cannot be placed: that is an error, for the caller to
// fail closed on.
func effectiveMarks(ctx context.Context, repos repo.TenantRepos, a domain.Asset, version string, depth int) ([]domain.SensitiveSegment, error) {
	marks, err := repos.Transcripts().Sensitive(ctx, id.Ref(a.ID), version)
	if err != nil {
		return nil, err
	}
	if a.Lineage == nil {
		return marks, nil
	}
	if depth >= maxLineageDepth {
		return nil, errs.New(errs.CodeFailedPrecondition, "a lineage is too deep to read its sensitive marks")
	}
	offset := 0.0
	for _, cut := range a.Lineage.Segments {
		span := cut.EndSeconds - cut.StartSeconds
		if span <= 0 {
			continue
		}
		start := offset
		offset += span
		sourceID := cut.SourceAssetID
		if sourceID == "" {
			sourceID = a.Lineage.SourceAssetID
		}
		if sourceID == "" {
			continue
		}
		source, err := repos.Assets().Get(ctx, id.Ref(sourceID))
		if err != nil {
			return nil, err
		}
		sourceVersion := cut.SourceVersion
		if sourceVersion == "" {
			sourceVersion = source.CurrentVersion
		}
		inherited, err := effectiveMarks(ctx, repos, source, sourceVersion, depth+1)
		if err != nil {
			return nil, err
		}
		for _, m := range inherited {
			from := max(m.StartSeconds, cut.StartSeconds)
			to := min(m.EndSeconds, cut.EndSeconds)
			if from >= to {
				continue
			}
			if version != a.CurrentVersion {
				return nil, errs.New(errs.CodeFailedPrecondition, "a source's sensitive mark cannot be placed on an older version's timeline")
			}
			marks = append(marks, domain.SensitiveSegment{
				AssetID:      a.ID,
				Version:      version,
				StartSeconds: start + from - cut.StartSeconds,
				EndSeconds:   start + to - cut.StartSeconds,
			})
		}
	}
	return marks, nil
}

// NewSearchGuard wraps service.Search for readers without asset.sensitive: a hit whose
// interval touches a sensitive mark is dropped, so search does not undo what
// Assets.Transcript redacts (D30, I118). Dropped rather than redacted, because a hit that
// survives with its text removed still says the searched word is in that interval.
func NewSearchGuard(next service.Search, stores repo.TenantStores) service.Search {
	return searchGuard{next, stores}
}

type searchGuard struct {
	next   service.Search
	stores repo.TenantStores
}

func (g searchGuard) Search(ctx context.Context, scope service.Scope, q api.SearchQuery) (api.SearchResponse, error) {
	resp, err := g.next.Search(ctx, scope, q)
	if err != nil || scope.Principal.Can(domain.PermAssetSensitive) {
		return resp, err
	}
	store, err := g.stores.ForTenant(ctx, scope.Tenant)
	if err != nil {
		return api.SearchResponse{}, err
	}
	type key struct {
		asset   id.Friendly
		version string
	}
	seen := map[key][]domain.SensitiveSegment{}
	kept := make([]domain.SearchHit, 0, len(resp.Items))
	for _, hit := range resp.Items {
		k := key{hit.AssetID, hit.Version}
		marks, ok := seen[k]
		if !ok {
			a, err := store.Assets().Get(ctx, id.Ref(hit.AssetID))
			if err != nil {
				return api.SearchResponse{}, err
			}
			marks, err = effectiveMarks(ctx, store, a, hit.Version, 0)
			if err != nil {
				return api.SearchResponse{}, err
			}
			seen[k] = marks
		}
		if !touchesMark(marks, hit.StartSeconds, hit.EndSeconds) {
			kept = append(kept, hit)
		}
	}
	resp.Items = kept
	return resp, nil
}

func touchesMark(marks []domain.SensitiveSegment, start, end float64) bool {
	for _, m := range marks {
		if start < m.EndSeconds && m.StartSeconds < end {
			return true
		}
	}
	return false
}

// refreshPublicExports asks for a public library refresh for an asset and everything
// derived from it, down the lineage. A mark on a source hides speech in its clips too,
// and those may be what is public (D30). Every public publication gets its event: the
// export rebuilds the whole file anyway, and marks are rare, human decisions.
func refreshPublicExports(ctx context.Context, tx repo.TenantRepos, scope service.Scope, a domain.Asset, reason string, at time.Time) error {
	visited := map[id.ULID]bool{a.ULID: true}
	queue := []domain.Asset{a}
	for len(queue) > 0 {
		current := queue[0]
		queue = queue[1:]
		if err := emitPublicUpdates(ctx, tx, scope, current, reason, at); err != nil {
			return err
		}
		page := repo.Page{Limit: 500}
		for {
			children, next, err := tx.Assets().List(ctx, repo.AssetFilter{SourceID: id.Ref(current.ULID)}, page)
			if err != nil {
				return err
			}
			for _, child := range children {
				if !visited[child.ULID] {
					visited[child.ULID] = true
					queue = append(queue, child)
				}
			}
			if next == "" {
				break
			}
			page.Cursor = next
		}
	}
	return nil
}
