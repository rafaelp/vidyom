package workers_test

import (
	"bufio"
	"context"
	"encoding/json"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/rafaelp/vidyom/internal/contract/domain"
	"github.com/rafaelp/vidyom/internal/contract/id"
	"github.com/rafaelp/vidyom/internal/contract/jobqueue"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg"
	"github.com/rafaelp/vidyom/internal/workers/ffmpeg/fixture"
	audionodes "github.com/rafaelp/vidyom/nodes/audio"
	storagenodes "github.com/rafaelp/vidyom/nodes/storage"
	videonodes "github.com/rafaelp/vidyom/nodes/video"
)

type accessEntry struct {
	Role  string `json:"role"`
	Op    string `json:"op"`
	Class string `json:"class"`
	Key   string `json:"key"`
}

func (r *rig) access() []accessEntry {
	r.t.Helper()
	f, err := os.Open(r.accessLog)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		r.t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	var out []accessEntry
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		var entry accessEntry
		if err := json.Unmarshal(scanner.Bytes(), &entry); err != nil {
			r.t.Fatal(err)
		}
		out = append(out, entry)
	}
	return out
}

func (r *rig) reads(role domain.StorageRole) []accessEntry {
	var out []accessEntry
	for _, entry := range r.access() {
		if entry.Role == string(role) && entry.Class == "read" {
			out = append(out, entry)
		}
	}
	return out
}

// step runs one node through the whole worker loop and returns its output.
func (r *rig) step(nodeID, nodeType string, params map[string]any) map[string]any {
	r.t.Helper()
	taskID, _ := r.submit(nodeID, nodeType, params)
	r.daemon.waitDone(r.t, taskID)
	out := r.daemon.completion(r.t, taskID)
	eventually(r.t, "the ack of "+nodeID, func() bool { return r.queue.isAcked(taskID) })
	return out
}

func registerMediaNodes(r *rig) {
	storagenodes.Register(r.registry)
	videonodes.Register(r.registry)
	audionodes.Register(r.registry)
}

func text(out map[string]any, key string) string {
	v, _ := out[key].(string)
	return v
}

// TestIngestAndClipThroughTheWorker follows §3.1 and §3.2, with the parameters of
// workflows/ingest-original.json and create-clip.json, on real FFmpeg, the real local
// driver and the real worker loop. Only the daemon and the queue are fakes.
func TestIngestAndClipThroughTheWorker(t *testing.T) {
	fixture.Require(t)
	r := newRig(t)
	r.cfg.ShutdownGrace = 10 * time.Second
	registerMediaNodes(r)
	r.start()

	// A recording arrives through the upload spool, named by ULID (I20, I183).
	upload := id.New()
	spool := filepath.Join(r.cfg.SpoolDir, string(r.tenant))
	if err := os.MkdirAll(spool, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(fixture.VFR(t, t.TempDir(), 4), filepath.Join(spool, string(upload))); err != nil {
		t.Fatal(err)
	}
	file := "upload:" + string(upload)
	prefix := "originals/ORI-001/v1"

	probe := r.step("probe", "video.probe", map[string]any{"source": file})
	if probe["vfr"] != true || text(probe, "ext") != "mp4" {
		t.Fatalf("probe %+v", probe)
	}
	backup := r.step("backup", "storage.put", map[string]any{
		"storage": "archive", "source": file, "key": prefix + "/original." + text(probe, "ext"), "verifyChecksum": true,
	})
	if backup["verified"] != true {
		t.Fatalf("backup %+v", backup)
	}
	master := r.step("compress", "video.transcode", map[string]any{"source": file, "profile": "auto:master"})
	if text(master, "profile") != "master-1080p@1" || master["height"] != 240.0 {
		t.Fatalf("master %+v", master)
	}
	storedMaster := r.step("store_master", "storage.put", map[string]any{
		"storage": "master", "source": text(master, "file"), "key": prefix + "/master.mp4", "verifyChecksum": true,
	})
	if text(storedMaster, "profile") != "master-1080p@1" || text(storedMaster, "sha256") != text(master, "sha256") {
		t.Fatalf("stored master %+v", storedMaster)
	}
	proxy := r.step("proxy", "video.transcode", map[string]any{"source": text(master, "file"), "profile": "proxy-720p"})
	storedProxy := r.step("store_proxy", "storage.put", map[string]any{
		"storage": "proxy", "source": text(proxy, "file"), "key": prefix + "/proxy.mp4", "verifyChecksum": true,
	})
	thumbnail := r.step("thumbnail", "video.thumbnail", map[string]any{"source": text(master, "file")})
	r.step("store_thumbnail", "storage.put", map[string]any{
		"storage": "proxy", "source": text(thumbnail, "file"), "key": prefix + "/thumbnail.jpg", "contentType": "image/jpeg",
	})
	// D56: from the stored master, through the verified cache.
	audio := r.step("audio", "audio.extract", map[string]any{"source": text(storedMaster, "uri"), "profile": "speech-opus"})
	timeline := r.step("timeline", "video.validate_timeline", map[string]any{
		"reference":  file,
		"candidates": []string{text(master, "file"), text(proxy, "file"), text(audio, "file")},
	})
	if timeline["valid"] != true {
		t.Fatalf("timeline %+v", timeline)
	}
	for _, duration := range timeline["durations"].(map[string]any) {
		if math.Abs(duration.(float64)-timeline["reference_duration"].(float64)) > timeline["tolerance_seconds"].(float64) {
			t.Fatalf("timeline %+v", timeline)
		}
	}
	// The whole ingest wrote a backup and never read media from archive (§7.2).
	if reads := r.reads(domain.RoleArchive); len(reads) != 0 {
		t.Fatalf("ingest read archive: %+v", reads)
	}

	// Later, a clip of the same original.
	if err := os.Truncate(r.accessLog, 0); err != nil {
		t.Fatal(err)
	}
	segments := []map[string]any{{"source_asset_id": "ORI-001", "source_version": "v1", "start_seconds": 0.5, "end_seconds": 2.0}}
	preview := r.step("preview", "video.preview", map[string]any{"source": text(storedProxy, "uri"), "segments": segments})
	if math.Abs(preview["duration_seconds"].(float64)-1.5) > 0.1 {
		t.Fatalf("preview %+v", preview)
	}
	if reads := r.reads(domain.RoleMaster); len(reads) != 0 {
		t.Fatalf("a preview read master: %+v", reads)
	}
	render := r.step("render", "video.cut", map[string]any{
		"source": text(storedMaster, "uri"), "segments": segments, "forbidStorageRoles": []string{"archive"},
	})
	if math.Abs(render["duration_seconds"].(float64)-1.5) > 0.1 {
		t.Fatalf("render %+v", render)
	}
	clipPrefix := "clips/CLIP-001/v1"
	r.step("clip_backup", "storage.put", map[string]any{
		"storage": "archive", "source": text(render, "file"), "key": clipPrefix + "/master.mp4", "verifyChecksum": true,
	})
	r.step("clip_store", "storage.put", map[string]any{
		"storage": "master", "source": text(render, "file"), "key": clipPrefix + "/master.mp4", "verifyChecksum": true,
	})
	r.step("clip_proxy", "video.transcode", map[string]any{"source": text(render, "file"), "profile": "proxy-720p"})

	// §1.1 step 6 and §15.2 item 3: the render read the master, and archive was only
	// written. The clip's own backup did not read it either (I186, I188).
	if reads := r.reads(domain.RoleArchive); len(reads) != 0 {
		t.Fatalf("clipping read archive: %+v", reads)
	}
	wroteArchive := false
	for _, entry := range r.access() {
		if entry.Role == "archive" && entry.Op == "put" && strings.HasPrefix(entry.Key, clipPrefix) {
			wroteArchive = true
		}
	}
	if !wroteArchive {
		t.Fatal("the clip was not backed up")
	}

	// The master was cached by the ASR extraction, so no new read reached storage.
	if reads := r.reads(domain.RoleMaster); len(reads) != 0 {
		t.Fatalf("the master was read again instead of reused from the cache: %+v", reads)
	}

	// Plaintext is gone with each task, while the job's own outputs remain.
	inputs, err := filepath.Glob(filepath.Join(r.cfg.ScratchDir, "acme", string(r.job), "*", ".input-*"))
	if err != nil || len(inputs) != 0 {
		t.Fatalf("plaintext inputs survived their tasks: %v %v", inputs, err)
	}
	info, err := ffmpeg.Default().Probe(context.Background(), filepath.Join(r.cfg.ScratchDir, "acme", string(r.job), "render", "clip.mp4"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Width != 320 {
		t.Fatalf("%+v", info)
	}

	// The cache holds no plaintext and names no key.
	entries, err := filepath.Glob(filepath.Join(r.cfg.CacheDir, "acme", "*.vcache"))
	if err != nil || len(entries) == 0 {
		t.Fatalf("cache entries %v %v", entries, err)
	}
	for _, entry := range entries {
		if strings.Contains(filepath.Base(entry), "master") {
			t.Fatalf("a cache file name reveals a key: %s", entry)
		}
	}

	// The render's usage reached the daemon for pricing.
	r.daemon.with(func() {
		req := r.daemon.completed[jobqueue.TaskID(r.job, "clip_store", 1)]
		if len(req.Usage) == 0 || req.Usage[0].Operation != "storage.ingress" || req.Usage[0].Provider != "local" {
			t.Errorf("usage %+v", req.Usage)
		}
		tools := r.daemon.completed[jobqueue.TaskID(r.job, "render", 1)].Tools
		if tools["ffmpeg"] == "" {
			t.Errorf("tools %+v", tools)
		}
	})

	// Job end: the cleanup pointer removes the job's scratch (D22, I45).
	cleanup := jobqueue.ScratchCleanupPointer(r.tenant, r.job, time.Now())
	if err := r.queue.Enqueue(context.Background(), cleanup); err != nil {
		t.Fatal(err)
	}
	eventually(t, "the job's scratch removal", func() bool {
		_, err := os.Stat(filepath.Join(r.cfg.ScratchDir, "acme", string(r.job)))
		return os.IsNotExist(err)
	})
}

// A cancel flag stops a real encode promptly, and it reports as cancelled (D11).
func TestCancelStopsARealEncode(t *testing.T) {
	fixture.Require(t)
	r := newRig(t)
	registerMediaNodes(r)
	root := t.TempDir()
	r.cfg.IngestRoots = []string{root}
	raw := fixture.Video(t, root, fixture.Spec{Seconds: 30, Width: 1280, Height: 720})
	taskID, tk := r.submit("compress", "video.transcode", map[string]any{"source": "server:" + raw, "profile": "master-1080p"})
	r.start()
	eventually(t, "the encode's first heartbeat", func() bool {
		var n int
		r.daemon.with(func() { n = r.daemon.taskBeats[taskID] })
		return n > 0
	})
	started := time.Now()
	r.daemon.with(func() { tk.cancel = true })
	r.daemon.waitDone(t, taskID)
	if took := time.Since(started); took > 15*time.Second {
		t.Fatalf("the encode took %s to stop", took)
	}
	if got := r.daemon.failure(t, taskID); got.Code != "cancelled" || got.Retryable {
		t.Fatalf("%+v", got)
	}
}
