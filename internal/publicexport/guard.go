package publicexport

import (
	"context"
	"regexp"
	"strings"

	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/repo"
)

// A ULID-shaped word. No title or transcript has a reason to hold one.
var ulidShape = regexp.MustCompile(`\b[0-9A-HJKMNP-TV-Z]{26}\b`)

// guardVideo is the last check on one video before it is written (R44): nothing
// about it may name a stored object or a technical ID. A hit drops this video and
// returns the reason, without the value. Dropping the video rather than failing the
// export means one bad title cannot stop a withdrawal from taking effect (I233).
//
// Friendly IDs are not refused here: a person may write "CLIP-001" in a description.
func guardVideo(ctx context.Context, store repo.TenantRepos, v *video) string {
	forbidden := []string{string(v.asset.ULID)}
	if versions, err := store.Versions().List(ctx, id.Ref(v.asset.ID)); err == nil {
		for _, ver := range versions {
			for _, f := range ver.Files {
				forbidden = append(forbidden, f.File.Key)
			}
		}
	} else {
		return "cannot read the asset's stored files to check for leaks"
	}

	clean := func(text string, isURL bool) bool {
		if strings.Contains(text, "storage://") {
			return false
		}
		if !isURL && ulidShape.MatchString(text) {
			return false
		}
		for _, f := range forbidden {
			if f != "" && strings.Contains(text, f) {
				return false
			}
		}
		return true
	}

	texts := []string{v.title, v.description, v.asset.Category, v.asset.Language}
	texts = append(texts, v.tags...)
	for _, c := range v.chapters {
		texts = append(texts, c.Title, c.Summary)
	}
	for _, s := range v.segments {
		texts = append(texts, s.Text)
	}
	for _, c := range v.vectors {
		texts = append(texts, c.Text)
	}
	for _, t := range texts {
		if !clean(t, false) {
			return "a released text field names a stored object or a technical ID"
		}
	}
	urls := []string{v.thumbnail}
	for _, l := range v.links {
		urls = append(urls, l.url, l.embed)
	}
	for _, u := range urls {
		if !clean(u, true) {
			return "a released URL names a stored object or a technical ID"
		}
	}
	return ""
}
