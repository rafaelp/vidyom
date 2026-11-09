package profiles

import (
	"testing"

	"github.com/rafaelp/vidyom/internal/contract/errs"
)

func TestEveryProfileLoadsAndIsVersioned(t *testing.T) {
	all, err := All()
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"master-1080p", "master-2160p-hevc", "proxy-720p", "speech-opus"}
	if len(all) != len(want) {
		t.Fatalf("got %d profiles", len(all))
	}
	for i, id := range want {
		if all[i].ID != id {
			t.Errorf("profile %d: %s, want %s", i, all[i].ID, id)
		}
		if all[i].Version < 1 || all[i].Ref() != id+"@1" {
			t.Errorf("%s: version %d, ref %s", id, all[i].Version, all[i].Ref())
		}
	}
}

func TestProfilesMatchTheSpec(t *testing.T) {
	cases := []struct {
		id        string
		codec     string
		crf       int
		maxHeight int
		audio     string
		bitrate   string
	}{
		{"master-1080p", "libx264", 18, 1080, "aac", "192k"},      // D14
		{"master-2160p-hevc", "libx265", 20, 2160, "aac", "192k"}, // D14
		{"proxy-720p", "libx264", 28, 720, "aac", "96k"},          // §4.2, D40
	}
	for _, c := range cases {
		p, err := Lookup(c.id)
		if err != nil {
			t.Fatal(err)
		}
		if p.Video == nil || p.Video.Codec != c.codec || p.Video.CRF != c.crf || p.Video.MaxHeight != c.maxHeight {
			t.Errorf("%s video: %+v", c.id, p.Video)
		}
		if p.Audio.Codec != c.audio || p.Audio.Bitrate != c.bitrate {
			t.Errorf("%s audio: %+v", c.id, p.Audio)
		}
		if !p.Faststart || p.Container != "mp4" {
			t.Errorf("%s container: %+v", c.id, p)
		}
	}
	speech, err := Lookup("speech-opus")
	if err != nil {
		t.Fatal(err)
	}
	if !speech.AudioOnly() || speech.Audio.Channels != 1 || speech.Audio.SampleRate != 16000 || speech.Audio.Codec != "libopus" {
		t.Errorf("speech-opus: %+v", speech)
	}
}

func TestAutoSelectsByEffectiveHeight(t *testing.T) {
	cases := map[int]string{
		240:  "master-1080p",
		720:  "master-1080p",
		1080: "master-1080p",
		1439: "master-1080p",
		1440: "master-2160p-hevc",
		2160: "master-2160p-hevc",
		4320: "master-2160p-hevc",
	}
	for height, want := range cases {
		p, err := Resolve("auto:master", height)
		if err != nil {
			t.Fatal(err)
		}
		if p.ID != want {
			t.Errorf("height %d: %s, want %s", height, p.ID, want)
		}
	}
}

func TestLookupRefusesWhatItCannotDeliver(t *testing.T) {
	if p, err := Lookup("master-1080p@1"); err != nil || p.ID != "master-1080p" {
		t.Fatalf("versioned lookup: %+v %v", p, err)
	}
	for _, name := range []string{"master-1080p@2", "nope", "auto:master"} {
		if _, err := Lookup(name); !errs.IsCode(err, errs.CodeInvalidArgument) {
			t.Errorf("%q: %v", name, err)
		}
	}
	if _, err := Resolve("auto:nothing", 1080); !errs.IsCode(err, errs.CodeInvalidArgument) {
		t.Errorf("unknown family: %v", err)
	}
}
