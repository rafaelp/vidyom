// Package profiles holds the versioned media profiles (§4.1, D14, D40, I25). The
// JSON files in this directory are embedded and checked against
// schemas/media-profile.v1.json when first loaded. Resolution is a ceiling, never
// a target (R17), and an asset records a profile as id@version.
package profiles

import (
	"embed"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"

	"github.com/rafaelp/vidyom/internal/contract/errs"
	"github.com/rafaelp/vidyom/schemas"
)

//go:embed *.json
var files embed.FS

// Families selected through auto:<family>.
const (
	FamilyMaster = "master"
	FamilyProxy  = "proxy"
	FamilySpeech = "speech"
)

// Video is a profile's video settings.
type Video struct {
	Codec       string `json:"codec"`
	Profile     string `json:"profile,omitempty"`
	Preset      string `json:"preset"`
	CRF         int    `json:"crf"`
	MaxHeight   int    `json:"maxHeight"`
	PixelFormat string `json:"pixelFormat,omitempty"`
}

// Audio is a profile's audio settings.
type Audio struct {
	Codec      string `json:"codec"`
	Bitrate    string `json:"bitrate"`
	Channels   int    `json:"channels,omitempty"`
	SampleRate int    `json:"sampleRate,omitempty"`
}

// Selector is the source height range a profile serves under auto:<family>.
// Height here means effective height, the shorter display side (I184).
type Selector struct {
	Family          string `json:"family"`
	MinSourceHeight int    `json:"minSourceHeight,omitempty"`
	MaxSourceHeight int    `json:"maxSourceHeight,omitempty"`
}

// Profile is one media profile.
type Profile struct {
	ID          string
	Version     int
	Description string
	Selector    Selector
	Video       *Video // nil for an audio-only profile
	Audio       Audio
	Container   string
	Faststart   bool
}

// Ref returns the id@version an asset records.
func (p Profile) Ref() string { return p.ID + "@" + strconv.Itoa(p.Version) }

// Family returns the family the profile belongs to.
func (p Profile) Family() string { return p.Selector.Family }

// AudioOnly reports whether the profile has no video.
func (p Profile) AudioOnly() bool { return p.Video == nil }

// Extension returns the output file extension without the dot.
func (p Profile) Extension() string { return p.Container }

type rawProfile struct {
	ID          string          `json:"id"`
	Version     int             `json:"version"`
	Description string          `json:"description"`
	Selector    *Selector       `json:"selector"`
	Video       json.RawMessage `json:"video"`
	Audio       Audio           `json:"audio"`
	Container   string          `json:"container"`
	Faststart   bool            `json:"faststart"`
}

func parse(data []byte) (Profile, error) {
	if err := schemas.Validate(schemas.MediaProfile, data); err != nil {
		return Profile{}, err
	}
	var raw rawProfile
	if err := json.Unmarshal(data, &raw); err != nil {
		return Profile{}, err
	}
	p := Profile{
		ID: raw.ID, Version: raw.Version, Description: raw.Description,
		Audio: raw.Audio, Container: raw.Container, Faststart: raw.Faststart,
	}
	if raw.Selector != nil {
		p.Selector = *raw.Selector
	}
	if len(raw.Video) > 0 && string(raw.Video) != "false" {
		var v Video
		if err := json.Unmarshal(raw.Video, &v); err != nil {
			return Profile{}, err
		}
		p.Video = &v
	}
	return p, nil
}

var load = sync.OnceValues(func() (map[string]Profile, error) {
	entries, err := files.ReadDir(".")
	if err != nil {
		return nil, err
	}
	out := make(map[string]Profile, len(entries))
	for _, entry := range entries {
		data, err := files.ReadFile(entry.Name())
		if err != nil {
			return nil, err
		}
		p, err := parse(data)
		if err != nil {
			return nil, fmt.Errorf("profile %s: %w", entry.Name(), err)
		}
		if entry.Name() != p.ID+".json" {
			return nil, fmt.Errorf("profile %s declares id %q", entry.Name(), p.ID)
		}
		out[p.ID] = p
	}
	return out, nil
})

// All returns every profile, sorted by ID.
func All() ([]Profile, error) {
	m, err := load()
	if err != nil {
		return nil, err
	}
	out := make([]Profile, 0, len(m))
	for _, p := range m {
		out = append(out, p)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].ID < out[b].ID })
	return out, nil
}

// Lookup finds a profile by "id" or "id@version". A named version other than the
// embedded one is refused, so an asset never records a profile it did not use.
func Lookup(name string) (Profile, error) {
	m, err := load()
	if err != nil {
		return Profile{}, errs.Wrap(errs.CodeInternal, err, "media profiles failed to load")
	}
	id, version, versioned := strings.Cut(name, "@")
	p, ok := m[id]
	if !ok {
		return Profile{}, errs.Newf(errs.CodeInvalidArgument, "unknown media profile %q", name)
	}
	if versioned && version != strconv.Itoa(p.Version) {
		return Profile{}, errs.Newf(errs.CodeInvalidArgument, "media profile %q is not available: the current version is %s", name, p.Ref())
	}
	return p, nil
}

// Auto picks the profile of a family that serves an effective height (I25, I184).
func Auto(family string, effectiveHeight int) (Profile, error) {
	all, err := All()
	if err != nil {
		return Profile{}, errs.Wrap(errs.CodeInternal, err, "media profiles failed to load")
	}
	for _, p := range all {
		if p.Selector.Family != family {
			continue
		}
		if p.Selector.MinSourceHeight > 0 && effectiveHeight < p.Selector.MinSourceHeight {
			continue
		}
		if p.Selector.MaxSourceHeight > 0 && effectiveHeight > p.Selector.MaxSourceHeight {
			continue
		}
		return p, nil
	}
	return Profile{}, errs.Newf(errs.CodeInvalidArgument, "no %s profile serves a source of height %d", family, effectiveHeight)
}

// Resolve turns a node's profile parameter, a name or auto:<family>, into a profile.
func Resolve(spec string, effectiveHeight int) (Profile, error) {
	if family, ok := strings.CutPrefix(spec, "auto:"); ok {
		return Auto(family, effectiveHeight)
	}
	return Lookup(spec)
}
