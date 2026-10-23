package handlers

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/metadata"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func TestJobJSONMetadataAvailable(t *testing.T) {
	f := newMetaFixture(t, nil, nil)
	get := func() map[string]any {
		w := doGet(f.r, "/job/"+f.job.ID, nil)
		var m map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	m := get()
	av, _ := m["metadataAvailable"].(map[string]any)
	if av == nil || av["original"] != false || av["result"] != true {
		t.Fatalf("no original file yet: %v", m["metadataAvailable"])
	}
	if _, has := m["strippedCount"]; has {
		t.Fatal("strippedCount must be omitted when 0")
	}
	f.writeOriginal(t, map[string]any{"fileType": "image"}) // legacy counts as available
	if av := get()["metadataAvailable"].(map[string]any); av["original"] != true {
		t.Fatalf("%v", av)
	}
	_ = os.Remove(f.result)
	if av := get()["metadataAvailable"].(map[string]any); av["result"] != false {
		t.Fatalf("missing result file: %v", av)
	}
	// The stored job is never mutated by the decoration.
	if f.job.MetadataAvailable != nil {
		t.Fatal("stored job was mutated")
	}
}

func TestJobJSONMetadataAvailableArchiveAndIncomplete(t *testing.T) {
	f := newMetaFixture(t, nil, map[string]interface{}{"mode": "image_restore"})
	_ = os.WriteFile(f.h.outputPath(f.job, f.outDir), []byte("t"), 0o644)
	if got := f.h.withMetadataFields(f.job).MetadataAvailable; got.Result {
		t.Fatalf("archive result: %+v", got)
	}
	job2 := f.jm.CreateJob(models.OriginalFileInfo{Name: "a.jpg", Type: "image/jpeg"}, map[string]interface{}{"format": "png"})
	if got := f.h.withMetadataFields(job2).MetadataAvailable; got.Result {
		t.Fatalf("incomplete: %+v", got)
	}
}

func TestRecordStrippedSetsCountAndCachesResult(t *testing.T) {
	f := newMetaFixture(t, nil, map[string]interface{}{"format": "jpg", "metadataMode": "strip"})
	ent := func(id, g, tag, cat string) metadata.MetadataEntry {
		return metadata.MetadataEntry{ID: id, Group: g, Tag: tag, Value: "v", Category: cat}
	}
	f.writeOriginal(t, newDocJSON(
		ent("System:FileName", "System", "FileName", "file"),
		ent("System:FileSize", "System", "FileSize", "file"),
		ent("GPS:GPSLatitude", "GPS", "GPSLatitude", "gps"),
		ent("GPS:GPSLongitude", "GPS", "GPSLongitude", "gps"),
		ent("IFD0:Make", "IFD0", "Make", "camera"),
		ent("Composite:ImageSize", "Composite", "ImageSize", "composite"),
	))
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument {
		return tempishDoc(p, k, ent("System:FileSize", "System", "FileSize", "file"))
	})
	f.h.recordStripped(f.job)
	if f.job.StrippedCount != 3 {
		t.Fatalf("strippedCount %d, want 3 (GPS x2 + Make; FileName/ImageSize are expected changes)", f.job.StrippedCount)
	}
	if probes.Load() != 1 {
		t.Fatalf("probes %d", probes.Load())
	}
	// First Compare is instant: the diff route serves the cached result.
	if w := doGet(f.r, "/job/"+f.job.ID+"/metadata/diff", nil); w.Code != 200 || probes.Load() != 1 {
		t.Fatalf("diff %d probes %d", w.Code, probes.Load())
	}
	w := doGet(f.r, "/job/"+f.job.ID, nil)
	if !strings.Contains(w.Body.String(), `"strippedCount":3`) {
		t.Fatalf("job json: %s", w.Body.String())
	}
}

func TestRecordStrippedNoopWhenNotStripAndFailureIsQuiet(t *testing.T) {
	f := newMetaFixture(t, nil, map[string]interface{}{"format": "jpg", "metadataMode": "keep"})
	probes := fakeInspect(t, func(p, k string) *metadata.MetadataDocument { return tempishDoc(p, k) })
	f.h.recordStripped(f.job)
	if probes.Load() != 0 || f.job.StrippedCount != 0 {
		t.Fatal("keep must not compute")
	}
	g := newMetaFixture(t, nil, map[string]interface{}{"format": "png", "removeMetadata": true})
	// No metadata.json: logged, count stays 0, no panic.
	g.h.recordStripped(g.job)
	if g.job.StrippedCount != 0 {
		t.Fatalf("%d", g.job.StrippedCount)
	}
}

func TestStripRequested(t *testing.T) {
	cases := []struct {
		typ  string
		opts map[string]interface{}
		want bool
	}{
		{"image/jpeg", map[string]interface{}{"metadataMode": "strip"}, true},
		{"image/jpeg", map[string]interface{}{"removeMetadata": true}, true},
		{"video/mp4", map[string]interface{}{"metadataMode": " Strip "}, true},
		{"audio/mpeg", map[string]interface{}{"metadataMode": "keep"}, false},
		{"video/mp4", map[string]interface{}{"removeMetadata": true}, false},
		{"image/jpeg", nil, false},
	}
	for _, tc := range cases {
		j := &models.ConversionJob{OriginalFile: models.OriginalFileInfo{Type: tc.typ}, Options: tc.opts}
		if got := stripRequested(j); got != tc.want {
			t.Errorf("%s %v: %v", tc.typ, tc.opts, got)
		}
	}
}
