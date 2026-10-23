package metadata

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

var update = flag.Bool("update", false, "rewrite testdata/*/want.json golden files")

var fixtures = []struct{ name, file, kind string }{
	{"jpeg-gps-xmp", "/tmp/mm_upload/IMG_0001.jpg", KindImage},
	{"mp4-quicktime-keys", "/tmp/mm_upload/clip.mov", KindVideo},
	{"pdf-info-xmp", "/tmp/mm_upload/report.pdf", KindDocument},
}

func inspectFixture(t *testing.T, name, file, kind string) []byte {
	t.Helper()
	in := &Inspector{Runner: fixtureRunner(name)}
	doc := in.Inspect(context.Background(), file, kind)
	b, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	return append(b, '\n')
}

func TestGolden(t *testing.T) {
	for _, f := range fixtures {
		t.Run(f.name, func(t *testing.T) {
			got := inspectFixture(t, f.name, f.file, f.kind)
			want := filepath.Join("testdata", f.name, "want.json")
			if *update {
				if err := os.WriteFile(want, got, 0o644); err != nil {
					t.Fatal(err)
				}
				return
			}
			exp, err := os.ReadFile(want)
			if err != nil {
				t.Fatal(err)
			}
			if string(exp) != string(got) {
				t.Fatalf("golden mismatch for %s (run go test ./internal/metadata -update)\n%s", f.name, got)
			}
		})
	}
}

func TestInspectDeterministic(t *testing.T) {
	for _, f := range fixtures {
		a := inspectFixture(t, f.name, f.file, f.kind)
		b := inspectFixture(t, f.name, f.file, f.kind)
		if string(a) != string(b) {
			t.Fatalf("%s: two runs differ", f.name)
		}
	}
}

func entryByID(doc *MetadataDocument, id string) *MetadataEntry {
	for i := range doc.Entries {
		if doc.Entries[i].ID == id {
			return &doc.Entries[i]
		}
	}
	return nil
}

func inspectDoc(t *testing.T, name, file, kind string) *MetadataDocument {
	t.Helper()
	return (&Inspector{Runner: fixtureRunner(name)}).Inspect(context.Background(), file, kind)
}

func TestModifyDateCollisionFixed(t *testing.T) {
	doc := inspectDoc(t, "jpeg-gps-xmp", "/x/IMG.jpg", KindImage)
	a, b := entryByID(doc, "IFD0:ModifyDate"), entryByID(doc, "XMP-xmp:ModifyDate")
	if a == nil || b == nil {
		t.Fatalf("missing ModifyDate entries: %v %v", a, b)
	}
	if a.Value == b.Value {
		t.Fatal("values should differ")
	}
}

func TestRawValueMergeAndBinary(t *testing.T) {
	doc := inspectDoc(t, "jpeg-gps-xmp", "/x/IMG.jpg", KindImage)
	lat := entryByID(doc, "GPS:GPSLatitude")
	if lat == nil || lat.RawValue == nil {
		t.Fatalf("GPSLatitude = %+v", lat)
	}
	if fn := entryByID(doc, "ExifIFD:FNumber"); fn.RawValue != nil {
		t.Fatalf("FNumber raw should be omitted when equal: %+v", fn)
	}
	th := entryByID(doc, "IFD1:ThumbnailImage")
	if th == nil || !th.Binary || th.Value != (BinaryValue{Bytes: 5120}) {
		t.Fatalf("thumbnail = %+v", th)
	}
}

func TestSkippedKeysAndNoDrops(t *testing.T) {
	for _, f := range fixtures {
		b, _ := os.ReadFile(filepath.Join("testdata", f.name, "exiftool.json"))
		pairs, err := decodeExiftoolJSON(string(b))
		if err != nil {
			t.Fatal(err)
		}
		skipped := 0
		for _, p := range pairs {
			group, tag, _ := splitKey(p.Key)
			if isSkippedKey(p.Key, group, tag) {
				skipped++
			}
		}
		ex := buildExiftoolEntries(pairs)
		if len(ex.entries) != len(pairs)-skipped {
			t.Fatalf("%s: entries %d != parsed %d - skipped %d", f.name, len(ex.entries), len(pairs), skipped)
		}
		for _, e := range ex.entries {
			if strings.Contains(e.ID, "Directory") || e.Tag == "ExifToolVersion" || e.Tag == "SourceFile" {
				t.Fatalf("leaked %s", e.ID)
			}
		}
	}
}

// Real exiftool -G1 output names the file-system group "System", not "File".
func TestSystemGroupTempFileTagsSkipped(t *testing.T) {
	pairs, err := decodeExiftoolJSON(`[{"SourceFile":"/srv/tmp/metadata_1_a.jpg","System:FileName":"metadata_1_a.jpg","System:Directory":"/srv/tmp","System:FileSize":"9 bytes","System:FileModifyDate":"2026:10:23 09:55:50-06:00","System:FileAccessDate":"2026:10:23 09:55:50-06:00","System:FileInodeChangeDate":"2026:10:23 09:55:50-06:00","System:FilePermissions":"-rw-r--r--","File:FileType":"JPEG"}]`)
	if err != nil {
		t.Fatal(err)
	}
	ex := buildExiftoolEntries(pairs)
	var ids []string
	for _, e := range ex.entries {
		ids = append(ids, e.ID)
	}
	want := []string{"System:FileName", "System:FileSize", "System:FileModifyDate", "File:FileType"}
	if strings.Join(ids, ",") != strings.Join(want, ",") {
		t.Fatalf("entries %v, want %v", ids, want)
	}
	if !IsUploadFileName(ex.entries[0]) || IsUploadFileName(ex.entries[1]) {
		t.Fatalf("IsUploadFileName wrong for %v", ids)
	}
}

func TestDuplicateKeysGetSuffix(t *testing.T) {
	pairs, err := decodeExiftoolJSON(`[{"IFD0:Make":"A","IFD0:Make":"B","XMP-x:Make":"C","IFD0:Make":"D"}]`)
	if err != nil {
		t.Fatal(err)
	}
	ex := buildExiftoolEntries(pairs)
	var ids []string
	for _, e := range ex.entries {
		ids = append(ids, e.ID)
	}
	if got := strings.Join(ids, ","); got != "IFD0:Make,IFD0:Make#2,XMP-x:Make,IFD0:Make#3" {
		t.Fatalf("ids = %s", got)
	}
}

func TestStructValuesKeepOrderAndNumbers(t *testing.T) {
	doc := inspectDoc(t, "jpeg-gps-xmp", "/x/IMG.jpg", KindImage)
	h := entryByID(doc, "XMP-xmpMM:History")
	b, _ := json.Marshal(h.Value)
	if !strings.HasPrefix(string(b), `[{"Action":"saved","SoftwareAgent"`) {
		t.Fatalf("history = %s", b)
	}
	v := entryByID(doc, "File:FileType")
	if v.Value != "JPEG" {
		t.Fatal(v.Value)
	}
	if iso := entryByID(doc, "ExifIFD:ISO"); iso.Value != json.Number("80") {
		t.Fatalf("iso %#v", iso.Value)
	}
}

func TestFFprobeFolding(t *testing.T) {
	doc := inspectDoc(t, "mp4-quicktime-keys", "/x/clip.mov", KindVideo)
	for _, id := range []string{"Format:duration", "Format:major_brand", "Stream:0:StreamType", "Stream:1:StreamType", "Stream:0:handler_name", "Chapter:0:title", "SideData:0:Display Matrix", "Keys:com.apple.quicktime.make"} {
		if entryByID(doc, id) == nil {
			t.Errorf("missing %s", id)
		}
	}
	if e := entryByID(doc, "Stream:0:StreamType"); e.Value != "video" || e.Category != "video" {
		t.Errorf("%+v", e)
	}
	if entryByID(doc, "Format:filename") != nil {
		t.Error("format filename leaked")
	}
	if !strings.HasPrefix(doc.Tools[len(doc.Tools)-1], "ffprobe 7.1") {
		t.Errorf("tools %v", doc.Tools)
	}
}

func TestFFprobeNotRunForImages(t *testing.T) {
	r := fixtureRunner("jpeg-gps-xmp")
	(&Inspector{Runner: r}).Inspect(context.Background(), "/x/a.jpg", KindImage)
	for _, c := range r.calls {
		if c[0] == "ffprobe" {
			t.Fatal("ffprobe called for image")
		}
	}
}

func okRunner(stdout string) *fakeRunner {
	return &fakeRunner{handler: func(context.Context, string, []string) (string, string, error) { return stdout, "", nil }}
}

func TestWarnings(t *testing.T) {
	validEx := `[{"File:FileType":"JPEG"}]`
	cases := []struct {
		name   string
		video  bool
		runner func() *fakeRunner
		want   string
	}{
		{"exiftool missing", false, func() *fakeRunner {
			return &fakeRunner{handler: func(context.Context, string, []string) (string, string, error) {
				return "", "", &exec.Error{Name: "exiftool", Err: exec.ErrNotFound}
			}}
		}, "exiftool unavailable"},
		{"exiftool timeout", false, func() *fakeRunner {
			return &fakeRunner{handler: func(ctx context.Context, _ string, _ []string) (string, string, error) {
				<-ctx.Done()
				return "", "", ctx.Err()
			}}
		}, "exiftool timed out after 20s"},
		{"exiftool exit", false, func() *fakeRunner {
			return &fakeRunner{handler: func(context.Context, string, []string) (string, string, error) {
				return "", "Error: File format error\nmore", errors.New("exit status 2")
			}}
		}, "exiftool failed: Error: File format error"},
		{"exiftool bad json", false, func() *fakeRunner { return okRunner("not json") }, "exiftool output could not be parsed"},
		{"numeric pass fails", false, func() *fakeRunner {
			return &fakeRunner{handler: func(_ context.Context, _ string, args []string) (string, string, error) {
				if hasArg(args, "-n") {
					return "", "Error: boom", errors.New("exit status 1")
				}
				return validEx, "", nil
			}}
		}, "exiftool numeric pass failed: Error: boom"},
		{"ffprobe exit", true, func() *fakeRunner {
			return &fakeRunner{handler: func(_ context.Context, tool string, _ []string) (string, string, error) {
				if tool == "ffprobe" {
					return "", "Invalid data found\nx", errors.New("exit status 1")
				}
				return validEx, "", nil
			}}
		}, "ffprobe failed: Invalid data found"},
		{"ffprobe bad json", true, func() *fakeRunner {
			return &fakeRunner{handler: func(_ context.Context, tool string, _ []string) (string, string, error) {
				if tool == "ffprobe" {
					return "{oops", "", nil
				}
				return validEx, "", nil
			}}
		}, "ffprobe output could not be parsed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			kind := KindImage
			if tc.video {
				kind = KindVideo
			}
			in := &Inspector{Runner: tc.runner(), Timeout: 30 * time.Millisecond}
			doc := in.Inspect(context.Background(), "/x/f", kind)
			if doc == nil || doc.Entries == nil || doc.Warnings == nil || doc.Tools == nil {
				t.Fatalf("document must be fully non-nil: %+v", doc)
			}
			found := false
			for _, w := range doc.Warnings {
				if w == tc.want {
					found = true
				}
			}
			if !found {
				t.Fatalf("warnings %q lack %q", doc.Warnings, tc.want)
			}
		})
	}
}

func TestPartialDocumentOnFFprobeFailureKeepsExiftool(t *testing.T) {
	r := &fakeRunner{handler: func(_ context.Context, tool string, _ []string) (string, string, error) {
		if tool == "ffprobe" {
			return "", "", errors.New("x")
		}
		return `[{"File:FileType":"MP4"}]`, "", nil
	}}
	doc := (&Inspector{Runner: r}).Inspect(context.Background(), "/x/a.mp4", KindVideo)
	if len(doc.Entries) != 1 || len(doc.Warnings) != 1 {
		t.Fatalf("%+v", doc)
	}
}

func TestEmptyDocumentJSONShape(t *testing.T) {
	doc := (&Inspector{Runner: okRunner(`[]`)}).Inspect(context.Background(), "/x/a.jpg", KindImage)
	b, _ := json.Marshal(doc)
	if !strings.Contains(string(b), `"entries":[]`) || !strings.Contains(string(b), `"warnings":[]`) {
		t.Fatalf("%s", b)
	}
}
