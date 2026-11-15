package persist

import (
	"atlas-render/internal/codec"
	"atlas-render/internal/model"
	"bytes"
	"errors"
	"image"
	"image/color"
	"path/filepath"
	"runtime"
	"testing"
	"time"
)

// fixture identifies the record used by the read/cleanup tests.
type fixture struct {
	snapshotID string
	jobID      string
	artifactID string
}

func testDocument() model.Document {
	return model.Document{Width: 8, Height: 8, Background: "#FFFFFF", Layers: []model.Layer{{
		ID: "plate", Kind: "rectangle", Width: 8, Height: 8,
		Transform: model.Transform{ScaleX: 1, ScaleY: 1},
		Opacity:   1, Fill: model.Paint{Kind: "solid", Color: "#FF0000"},
	}}}
}

func onePixelPNG() []byte {
	img := image.NewNRGBA(image.Rect(0, 0, 1, 1))
	img.SetNRGBA(0, 0, color.NRGBA{R: 255, A: 255})
	data, e := codec.Encode(img)
	if e != nil {
		panic(e)
	}
	return data
}

// seed pins one succeeded job to a live PNG and surrounds it with unrelated
// scenes, assets and results. That unrelated state is what a whole-state read
// used to copy for a single download or comparison.
func seed(s *model.State, png []byte, extraJobs, extraAssets, extraScenes int) fixture {
	now := time.Now().UTC()
	doc := testDocument()
	snap := model.Snapshot{ID: "snap-1", SceneID: "scene-1", Revision: 1, State: "active", Document: doc, Assets: map[string]model.Asset{}, CreatedAt: now}
	snap.Digest = model.SnapshotDigest(snap)
	s.Scenes["scene-1"] = model.Scene{ID: "scene-1", Name: "fixture", Revision: 1, State: "active", Document: doc, UpdatedAt: now}
	s.Snapshots[snap.ID] = snap
	f := fixture{snapshotID: snap.ID, jobID: "job-1", artifactID: "png-1"}
	s.Jobs[f.jobID] = model.Job{ID: f.jobID, SnapshotID: snap.ID, State: "succeeded", Scale: 1, Attempt: 1, ArtifactID: f.artifactID, CreatedAt: now, UpdatedAt: now, Payload: snap}
	s.Artifacts[f.artifactID] = model.Artifact{ID: f.artifactID, JobID: f.jobID, State: "live", Digest: model.Digest(png), Width: doc.Width, Height: doc.Height, PNG: png}

	assetPNG := onePixelPNG()
	for i := 0; i < extraAssets; i++ {
		id := "asset-" + itoa(i)
		s.Assets[id] = model.Asset{ID: id, Digest: model.Digest(assetPNG), State: "available", Width: 1, Height: 1, PNG: assetPNG}
	}
	for i := 0; i < extraScenes; i++ {
		id := "scene-" + itoa(i)
		s.Scenes[id] = model.Scene{ID: id, Name: "bystander", Revision: 1, State: "active", Document: doc, UpdatedAt: now}
	}
	for i := 0; i < extraJobs; i++ {
		jobID := "job-x-" + itoa(i)
		artID := "png-x-" + itoa(i)
		data := bytes.Repeat([]byte{byte(0xA0 + i%32)}, 16<<10)
		s.Jobs[jobID] = model.Job{ID: jobID, SnapshotID: snap.ID, State: "succeeded", Scale: 1, Attempt: 1, ArtifactID: artID, CreatedAt: now, UpdatedAt: now, Payload: snap}
		s.Artifacts[artID] = model.Artifact{ID: artID, JobID: jobID, State: "live", Digest: model.Digest(data), Width: doc.Width, Height: doc.Height, PNG: data}
	}
	return f
}

func itoa(v int) string {
	if v == 0 {
		return "0"
	}
	var b [20]byte
	i := len(b)
	for v > 0 {
		i--
		b[i] = byte('0' + v%10)
		v /= 10
	}
	return string(b[i:])
}

func openSeeded(t testing.TB, png []byte, extraJobs int) (*Store, fixture) {
	t.Helper()
	st, e := Open(filepath.Join(t.TempDir(), "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = st.Close() })
	var f fixture
	if e := st.Update(func(s *model.State) error { f = seed(s, png, extraJobs, 200, 100); return nil }); e != nil {
		t.Fatal(e)
	}
	return st, f
}

func purge(f fixture) func(*model.State) error {
	return func(s *model.State) error {
		a := s.Artifacts[f.artifactID]
		a.State = "purged"
		a.PNG = nil
		s.Artifacts[f.artifactID] = a
		s.Emit("artifact.purged", f.artifactID)
		return nil
	}
}

func TestArtifactReadIsDetachedFromLaterPurge(t *testing.T) {
	png := bytes.Repeat([]byte{9, 8, 7, 6}, 64)
	st, f := openSeeded(t, png, 0)

	got, ok := st.Artifact(f.artifactID)
	if !ok || !bytes.Equal(got.PNG, png) || got.Digest != model.Digest(png) {
		t.Fatalf("unexpected artifact read: ok=%v bytes=%d", ok, len(got.PNG))
	}
	got.PNG[0] ^= 0xFF // the reader owns this copy; the store must not see the edit
	again, _ := st.Artifact(f.artifactID)
	if !bytes.Equal(again.PNG, png) {
		t.Fatal("mutating a returned artifact changed persisted bytes")
	}
	// Keep the bytes handed out before cleanup.
	before, _ := st.Artifact(f.artifactID)
	if e := st.Update(purge(f)); e != nil {
		t.Fatal(e)
	}
	after, ok := st.Artifact(f.artifactID)
	if !ok || after.State != "purged" || len(after.PNG) != 0 {
		t.Fatalf("purge did not clear the stored record: %+v", after)
	}
	if !bytes.Equal(before.PNG, png) || before.Digest != model.Digest(png) {
		t.Fatal("purge altered bytes already handed to a reader")
	}
	if _, ok := st.Artifact("missing"); ok {
		t.Fatal("missing artifact reported as present")
	}
}

func TestFailedCommitLeavesArtifactAndSuccessFactUnchanged(t *testing.T) {
	png := bytes.Repeat([]byte{1, 2, 3, 4}, 128)
	st, f := openSeeded(t, png, 0)

	readBefore, _ := st.Artifact(f.artifactID)
	write := st.write
	st.write = func(string, []byte) error { return errors.New("disk full") }
	if e := st.Update(purge(f)); e == nil {
		t.Fatal("purge reported success although the commit failed")
	}
	st.write = write

	live, ok := st.Artifact(f.artifactID)
	if !ok || live.State != "live" || !bytes.Equal(live.PNG, png) || live.Digest != model.Digest(png) {
		t.Fatalf("failed commit changed the readable artifact: %+v", live)
	}
	if live.Digest != readBefore.Digest {
		t.Fatal("failed commit changed the conditional-download validator")
	}
	state := st.Read()
	if state.Jobs[f.jobID].State != "succeeded" {
		t.Fatal("failed commit changed the success fact")
	}
	if !bytes.Equal(state.Artifacts[f.artifactID].PNG, png) {
		t.Fatal("failed commit lost the artifact bytes")
	}
}

func TestPurgeDoesNotWaitForConsumer(t *testing.T) {
	png := bytes.Repeat([]byte{5, 6, 7, 8}, 128)
	st, f := openSeeded(t, png, 0)
	held, ok := st.Artifact(f.artifactID)
	if !ok {
		t.Fatal("artifact missing")
	}
	// Simulate a slow consumer: it owns the bytes and blocks before writing
	// them to a client or decoding them for comparison.
	release := make(chan struct{})
	started := make(chan struct{})
	go func() {
		close(started)
		<-release
		_ = held.PNG[0]
	}()
	<-started
	done := make(chan error, 1)
	go func() { done <- st.Update(purge(f)) }()
	select {
	case e := <-done:
		if e != nil {
			t.Fatal(e)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("purge waited for a consumer holding result bytes")
	}
	close(release)
}

func allocatedBytes(f func()) uint64 {
	runtime.GC()
	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	f()
	runtime.ReadMemStats(&after)
	return after.TotalAlloc - before.TotalAlloc
}

func minAllocatedBytes(f func()) uint64 {
	best := ^uint64(0)
	for i := 0; i < 3; i++ {
		v := allocatedBytes(f)
		if v < best {
			best = v
		}
	}
	return best
}

// TestArtifactReadCopiesOnlyRequestedBytes demonstrates that a result read no
// longer scales with unrelated state. The fixture holds one 16KiB result among
// 200 unrelated 16KiB results plus bystander scenes and assets.
func TestArtifactReadCopiesOnlyRequestedBytes(t *testing.T) {
	png := bytes.Repeat([]byte{0x5A}, 16<<10)
	st, f := openSeeded(t, png, 200)

	one := minAllocatedBytes(func() { st.Artifact(f.artifactID) })
	many := minAllocatedBytes(func() { st.Artifact(f.artifactID); st.Artifact("png-x-7") })
	// A comparison reads two results; it must not pay for the whole state.
	if many > 4*uint64(2*len(png)) {
		t.Fatalf("reading two results allocated %d bytes for %d bytes of image data", many, 2*len(png))
	}
	full := minAllocatedBytes(func() { st.Read() })
	if many*8 >= full {
		t.Fatalf("two result reads allocated %d bytes; a whole-state read allocated only %d", many, full)
	}
	t.Logf("two-result read allocated %d bytes; whole-state read allocated %d bytes (%.1fx)", many, full, float64(full)/float64(one))
}

func benchmarkStore(b *testing.B, unrelatedJobs int) (*Store, fixture) {
	b.Helper()
	st, e := Open(filepath.Join(b.TempDir(), "state.json"))
	if e != nil {
		b.Fatal(e)
	}
	b.Cleanup(func() { _ = st.Close() })
	var f fixture
	png := bytes.Repeat([]byte{0x42}, 16<<10)
	if e := st.Update(func(s *model.State) error { f = seed(s, png, unrelatedJobs, 200, 100); return nil }); e != nil {
		b.Fatal(e)
	}
	return st, f
}

func BenchmarkArtifactRead(b *testing.B) {
	st, f := benchmarkStore(b, 200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, ok := st.Artifact(f.artifactID); !ok {
			b.Fatal("artifact missing")
		}
	}
}

func BenchmarkFullStateRead(b *testing.B) {
	st, _ := benchmarkStore(b, 200)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_ = st.Read()
	}
}
