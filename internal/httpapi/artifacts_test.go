package httpapi

import (
	"atlas-render/internal/codec"
	"atlas-render/internal/engine"
	"atlas-render/internal/model"
	"atlas-render/internal/observe"
	"atlas-render/internal/persist"
	"atlas-render/internal/service"
	"bytes"
	"encoding/json"
	"image"
	"image/color"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func fixturePNG(t *testing.T) []byte {
	t.Helper()
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: uint8(20 * x), G: uint8(30 * y), B: 90, A: 255})
		}
	}
	data, e := codec.Encode(img)
	if e != nil {
		t.Fatal(e)
	}
	return data
}

// artifactServer seeds one succeeded job whose result is surrounded by
// unrelated state, then serves the real HTTP handler.
func artifactServer(t *testing.T) (*httptest.Server, *persist.Store, []byte) {
	t.Helper()
	st, e := persist.Open(filepath.Join(t.TempDir(), "state.json"))
	if e != nil {
		t.Fatal(e)
	}
	t.Cleanup(func() { _ = st.Close() })
	png := fixturePNG(t)
	now := time.Now().UTC()
	doc := model.Document{Width: 4, Height: 4, Background: "#FFFFFF", Layers: []model.Layer{{
		ID: "plate", Kind: "rectangle", Width: 4, Height: 4,
		Transform: model.Transform{ScaleX: 1, ScaleY: 1},
		Opacity:   1, Fill: model.Paint{Kind: "solid", Color: "#FF0000"},
	}}}
	if e := st.Update(func(s *model.State) error {
		snap := model.Snapshot{ID: "snap-1", SceneID: "scene-1", Revision: 1, State: "active", Document: doc, Assets: map[string]model.Asset{}, CreatedAt: now}
		snap.Digest = model.SnapshotDigest(snap)
		s.Scenes["scene-1"] = model.Scene{ID: "scene-1", Name: "fixture", Revision: 1, State: "active", Document: doc, UpdatedAt: now}
		s.Snapshots[snap.ID] = snap
		s.Jobs["job-1"] = model.Job{ID: "job-1", SnapshotID: snap.ID, State: "succeeded", Scale: 1, Attempt: 1, ArtifactID: "png-1", CreatedAt: now, UpdatedAt: now, Payload: snap}
		s.Artifacts["png-1"] = model.Artifact{ID: "png-1", JobID: "job-1", State: "live", Digest: model.Digest(png), Width: 4, Height: 4, PNG: png}
		return nil
	}); e != nil {
		t.Fatal(e)
	}
	srv := httptest.NewServer(API{Service: &service.Service{Store: st}, Worker: engine.New(st)}.Handler())
	t.Cleanup(srv.Close)
	return srv, st, png
}

func TestDownloadKeepsETagBytesAndSuccessFactAcrossPurge(t *testing.T) {
	srv, st, png := artifactServer(t)
	etag := `"` + model.Digest(png) + `"`

	resp, e := http.Get(srv.URL + "/v1/artifacts/png-1")
	if e != nil {
		t.Fatal(e)
	}
	body := make([]byte, len(png))
	n, _ := resp.Body.Read(body)
	resp.Body.Close()
	if resp.StatusCode != 200 || n != len(png) || !bytes.Equal(body[:n], png) {
		t.Fatalf("download returned status %d and %d bytes", resp.StatusCode, n)
	}
	if got := resp.Header.Get("ETag"); got != etag {
		t.Fatalf("ETag = %q, want %q", got, etag)
	}

	req, _ := http.NewRequest("GET", srv.URL+"/v1/artifacts/png-1", nil)
	req.Header.Set("If-None-Match", etag)
	resp, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusNotModified {
		t.Fatalf("conditional download returned %d, want 304", resp.StatusCode)
	}

	// A reader that already received the bytes keeps them even when cleanup
	// commits while the response is still being produced.
	held, e := (&service.Service{Store: st}).Artifact("png-1")
	if e != nil {
		t.Fatal(e)
	}
	req, _ = http.NewRequest("DELETE", srv.URL+"/v1/artifacts/png-1", nil)
	resp, e = http.DefaultClient.Do(req)
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("purge returned %d", resp.StatusCode)
	}
	if !bytes.Equal(held.PNG, png) || held.Digest != model.Digest(png) {
		t.Fatal("purge changed bytes already handed to a reader")
	}

	resp, e = http.Get(srv.URL + "/v1/artifacts/png-1")
	if e != nil {
		t.Fatal(e)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusGone {
		t.Fatalf("download after purge returned %d, want 410", resp.StatusCode)
	}
	jobResp, e := http.Get(srv.URL + "/v1/jobs/job-1")
	if e != nil {
		t.Fatal(e)
	}
	defer jobResp.Body.Close()
	if jobResp.StatusCode != 200 {
		t.Fatalf("job read after purge returned %d", jobResp.StatusCode)
	}
	var got model.Job
	if e := json.NewDecoder(jobResp.Body).Decode(&got); e != nil {
		t.Fatal(e)
	}
	if got.State != "succeeded" {
		t.Fatalf("purge changed the success fact to %q", got.State)
	}
}

// TestCompareKeepsDecodedBytesWhenPurgeInterleaves interleaves a pixel
// comparison with cleanup. The comparison reads both artifacts up front, so it
// must finish on its own copies even though cleanup commits in between, and
// cleanup must not block on the (slow) decode and comparison work.
func TestCompareKeepsDecodedBytesWhenPurgeInterleaves(t *testing.T) {
	_, st, png := artifactServer(t)
	img := image.NewNRGBA(image.Rect(0, 0, 4, 4))
	for y := 0; y < 4; y++ {
		for x := 0; x < 4; x++ {
			img.SetNRGBA(x, y, color.NRGBA{R: 255 - uint8(20*x), G: uint8(30 * y), B: 90, A: 255})
		}
	}
	other, e := codec.Encode(img)
	if e != nil {
		t.Fatal(e)
	}
	if e := st.Update(func(s *model.State) error {
		a := s.Artifacts["png-1"]
		a.ID, a.JobID = "png-2", "job-2"
		a.Digest, a.PNG = model.Digest(other), other
		s.Artifacts[a.ID] = a
		j := s.Jobs["job-1"]
		j.ID, j.ArtifactID = "job-2", "png-2"
		s.Jobs[j.ID] = j
		return nil
	}); e != nil {
		t.Fatal(e)
	}

	svc := &service.Service{Store: st}
	left, e := svc.Artifact("png-1")
	if e != nil {
		t.Fatal(e)
	}
	right, e := svc.Artifact("png-2")
	if e != nil {
		t.Fatal(e)
	}
	if e := svc.Purge("png-1"); e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(left.PNG, png) {
		t.Fatal("purge changed the left result before comparison")
	}
	cmp, e := observe.Compare(left, right)
	if e != nil {
		t.Fatalf("comparison failed after interleaved purge: %v", e)
	}
	if cmp.ChangedPixels == 0 {
		t.Fatal("comparison ignored the visible difference")
	}
	if _, e := svc.Artifact("png-1"); e == nil {
		t.Fatal("purged artifact still downloadable")
	} else if de, ok := e.(*model.Error); !ok || de.Status != 410 {
		t.Fatalf("purged artifact returned %v, want 410", e)
	}
}
