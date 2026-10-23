package handlers

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/services"
)

type recRunner struct {
	calls [][]string
	out   string
	err   error
}

func (r *recRunner) Run(_ context.Context, name string, args ...string) (string, string, error) {
	r.calls = append(r.calls, append([]string{name}, args...))
	return r.out, "", r.err
}

func useBinaryRunner(t *testing.T, r metadata.Runner) {
	t.Helper()
	prev := binaryRunner
	binaryRunner = r
	t.Cleanup(func() { binaryRunner = prev })
}

func binDoc() metadata.MetadataDocument {
	return newDocJSON(
		metadata.MetadataEntry{ID: "IFD1:ThumbnailImage", Group: "IFD1", Tag: "ThumbnailImage", Value: metadata.BinaryValue{Bytes: 10}, Binary: true, Category: "thumbnail"},
		metadata.MetadataEntry{ID: "IFD0:Make", Group: "IFD0", Tag: "Make", Value: "Apple", Category: "camera"},
		metadata.MetadataEntry{ID: "ICC_Profile:ProfileDescription", Group: "ICC_Profile", Tag: "ProfileDescription", Value: metadata.BinaryValue{Bytes: 4}, Binary: true, Category: "color"},
	)
}

func binFixture(t *testing.T, owner *services.JobOwner) *metaFixture {
	f := newMetaFixture(t, owner, nil)
	f.writeOriginal(t, binDoc())
	_ = os.MkdirAll(f.upDir, 0o755)
	_ = os.WriteFile(filepath.Join(f.upDir, "original_photo.jpg"), []byte("x"), 0o644)
	return f
}

func TestBinaryRouteExtractsWithDoubleDash(t *testing.T) {
	jpeg := "\xff\xd8\xff\xe0" + strings.Repeat("a", 100)
	rr := &recRunner{out: jpeg}
	useBinaryRunner(t, rr)
	f := binFixture(t, nil)
	id := url.PathEscape("IFD1:ThumbnailImage")

	w := doGet(f.r, "/job/"+f.job.ID+"/metadata/binary/"+id, nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if len(rr.calls) != 1 {
		t.Fatalf("calls %v", rr.calls)
	}
	call := rr.calls[0]
	want := []string{"exiftool", "-b", "-IFD1:ThumbnailImage", "--", filepath.Join(f.upDir, "original_photo.jpg")}
	if strings.Join(call, "|") != strings.Join(want, "|") {
		t.Fatalf("args %v want %v", call, want)
	}
	if w.Header().Get("X-Content-Type-Options") != "nosniff" || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Content-Type") != "image/jpeg" {
		t.Fatalf("headers %v", w.Header())
	}
	w = doGet(f.r, "/job/"+f.job.ID+"/metadata/binary/"+id+"?inline=1", nil)
	if !strings.HasPrefix(w.Header().Get("Content-Disposition"), "inline") {
		t.Fatalf("inline: %v", w.Header())
	}
	// Non-image bytes: attachment octet-stream even with inline=1.
	rr.out = "just some bytes that are not an image"
	w = doGet(f.r, "/job/"+f.job.ID+"/metadata/binary/"+id+"?inline=1", nil)
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Disposition"), "attachment") || w.Header().Get("Content-Type") != "application/octet-stream" {
		t.Fatalf("non-image: %d %v", w.Code, w.Header())
	}
}

func TestBinaryRouteRejectsBadIDs(t *testing.T) {
	rr := &recRunner{out: "x"}
	useBinaryRunner(t, rr)
	f := binFixture(t, nil)
	bad := map[string]int{
		"IFD1:Thumbnail%20Image":    400, // space
		"IFD1:..%2Fetc":             404, // encoded slash never reaches the handler
		"..:Thumb":                  400,
		"-IFD1:ThumbnailImage":      400, // leading dash
		"IFD1:-ThumbnailImage":      400,
		"IFD1:ThumbnailImage%23abc": 400, // '#' + non-digits
		"IFD1:Thumb;rm":             400,
		"nocolon":                   400,
		"IFD0:Make":                 404, // valid id, not binary
		"IFD1:Missing":              404, // valid id, not in document
		"IFD1:ThumbnailImage%232":   422, // duplicate suffix not extractable
	}
	for id, want := range bad {
		w := doGet(f.r, "/job/"+f.job.ID+"/metadata/binary/"+id, nil)
		if w.Code != want {
			t.Errorf("%q: %d want %d (%s)", id, w.Code, want, w.Body.String())
		}
	}
	if len(rr.calls) != 0 {
		t.Fatalf("exiftool must not run for rejected ids: %v", rr.calls)
	}
}

func TestBinaryRouteSizeCapAndFailures(t *testing.T) {
	rr := &recRunner{out: strings.Repeat("a", maxBinaryEntryBytes+1)}
	useBinaryRunner(t, rr)
	f := binFixture(t, nil)
	target := "/job/" + f.job.ID + "/metadata/binary/IFD1:ThumbnailImage"
	if w := doGet(f.r, target, nil); w.Code != 413 {
		t.Fatalf("cap: %d", w.Code)
	}
	rr.out, rr.err = "", context.DeadlineExceeded
	if w := doGet(f.r, target, nil); w.Code != 504 {
		t.Fatalf("timeout: %d", w.Code)
	}
	rr.err = os.ErrNotExist
	if w := doGet(f.r, target, nil); w.Code != 502 {
		t.Fatalf("fail: %d", w.Code)
	}
	rr.err = nil
	if w := doGet(f.r, target, nil); w.Code != 404 {
		t.Fatalf("empty: %d", w.Code)
	}
	// Original upload gone.
	_ = os.RemoveAll(f.upDir)
	rr.out = "abc"
	if w := doGet(f.r, target, nil); w.Code != 404 {
		t.Fatalf("upload gone: %d", w.Code)
	}
}

func TestBinaryRouteResultSide(t *testing.T) {
	rr := &recRunner{out: "\x89PNG\r\n\x1a\n" + strings.Repeat("b", 50)}
	useBinaryRunner(t, rr)
	f := newMetaFixture(t, nil, nil)
	fakeInspect(t, func(p, k string) *metadata.MetadataDocument {
		return tempishDoc(p, k, metadata.MetadataEntry{ID: "IFD1:ThumbnailImage", Group: "IFD1", Tag: "ThumbnailImage", Value: metadata.BinaryValue{Bytes: 3}, Binary: true, Category: "thumbnail"})
	})
	w := doGet(f.r, "/job/"+f.job.ID+"/metadata/binary/IFD1:ThumbnailImage?side=result", nil)
	if w.Code != 200 || len(rr.calls) != 1 || rr.calls[0][len(rr.calls[0])-1] != f.result || rr.calls[0][len(rr.calls[0])-2] != "--" {
		t.Fatalf("%d %v", w.Code, rr.calls)
	}
	_ = os.Remove(f.result)
	if w := doGet(f.r, "/job/"+f.job.ID+"/metadata/binary/IFD1:ThumbnailImage?side=result", nil); w.Code != 404 {
		t.Fatalf("result gone: %d", w.Code)
	}
}

func TestBinaryAndDiffOwnership(t *testing.T) {
	useBinaryRunner(t, &recRunner{out: "\xff\xd8\xff\xe0abc"})
	fakeInspect(t, func(p, k string) *metadata.MetadataDocument { return tempishDoc(p, k) })
	f := binFixture(t, &services.JobOwner{SessionID: "sess-owner"})
	ownershipMatrix(t, f, "/job/"+f.job.ID+"/metadata/binary/IFD1:ThumbnailImage", 200)
	ownershipMatrix(t, f, "/job/"+f.job.ID+"/metadata/diff", 200)
}

func TestJobMetadataDiffRoute(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	f.writeOriginal(t, newDocJSON(
		metadata.MetadataEntry{ID: "System:FileName", Group: "System", Tag: "FileName", Value: "photo.jpg", Category: "file"},
		metadata.MetadataEntry{ID: "GPS:GPSLatitude", Group: "GPS", Tag: "GPSLatitude", Value: "1 N", Category: "gps"},
		metadata.MetadataEntry{ID: "IFD0:Make", Group: "IFD0", Tag: "Make", Value: "Apple", Category: "camera"},
	))
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument {
		return tempishDoc(p, k, metadata.MetadataEntry{ID: "IFD0:Make", Group: "IFD0", Tag: "Make", Value: "Apple", Category: "camera"})
	})
	w := doGet(f.r, "/job/"+f.job.ID+"/metadata/diff", nil)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	var d metadata.DiffResult
	if err := json.Unmarshal(w.Body.Bytes(), &d); err != nil {
		t.Fatal(err)
	}
	if len(d.Removed) != 1 || d.Removed[0].ID != "GPS:GPSLatitude" || d.Unchanged != 1 || len(d.Changed) != 1 || d.Changed[0].ID != "System:FileName" || len(d.Added) != 0 {
		t.Fatalf("diff %s", w.Body.String())
	}
	// The temp-vs-upload name difference is not reported as a change.
	// Both sides carry user-facing names (never the server temp names); the
	// name change is the one expected-to-change row, still reported.
	if d.Changed[0].Before != "photo.jpg" || d.Changed[0].After != f.h.getOutputFilename(f.job) || bytes.Contains(w.Body.Bytes(), []byte(f.outDir)) || probes.Load() != 1 {
		t.Fatalf("body %s probes %d", w.Body.String(), probes.Load())
	}
}

func TestJobMetadataDiffConflicts(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	fakeInspect(t, func(p, k string) *metadata.MetadataDocument { return tempishDoc(p, k) })
	f.writeOriginal(t, newDocJSON())
	_ = os.Remove(f.result)
	w := doGet(f.r, "/job/"+f.job.ID+"/metadata/diff", nil)
	if w.Code != 409 || !strings.Contains(w.Body.String(), `"error"`) {
		t.Fatalf("result missing: %d %s", w.Code, w.Body.String())
	}
	g := newMetaFixture(t, nil, map[string]interface{}{"mode": "image_restore"})
	g.writeOriginal(t, newDocJSON())
	_ = os.WriteFile(g.h.outputPath(g.job, g.outDir), []byte("t"), 0o644)
	if w := doGet(g.r, "/job/"+g.job.ID+"/metadata/diff", nil); w.Code != 409 {
		t.Fatalf("archive: %d", w.Code)
	}
}
