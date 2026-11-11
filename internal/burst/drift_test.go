package burst

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/manifest"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

type fakeImages struct {
	tags  map[string]map[string]string // ami -> tags
	err   error
	calls int
}

func (f *fakeImages) DescribeImages(_ context.Context, in *ec2.DescribeImagesInput, _ ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error) {
	f.calls++
	if f.err != nil {
		return nil, f.err
	}
	var out ec2.DescribeImagesOutput
	for _, id := range in.ImageIds {
		tg, ok := f.tags[id]
		if !ok {
			continue
		}
		im := types.Image{ImageId: aws.String(id)}
		for k, v := range tg {
			im.Tags = append(im.Tags, types.Tag{Key: aws.String(k), Value: aws.String(v)})
		}
		out.Images = append(out.Images, im)
	}
	return &out, nil
}

func hostOf(m map[string]string) HostManifest {
	return func(f string) (manifest.Manifest, error) {
		if v, ok := m[f]; ok {
			return manifest.Manifest{Family: f, SHA256: v}, nil
		}
		return manifest.Manifest{}, errors.New("no such family on this fake host")
	}
}

func TestDriftMatchStaleMissingAndUntagged(t *testing.T) {
	api := &fakeImages{tags: map[string]map[string]string{
		"ami-image":   {TagRuntimeManifest: "h1"},
		"ami-audio":   {TagRuntimeManifest: "OLD"},
		"ami-video":   {},
		"ami-restore": nil,
	}}
	delete(api.tags, "ami-restore") // not found at all
	dc := &DriftChecker{API: api, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		AMIs: map[string]string{"image:x86_64": "ami-image", "audio:x86_64": "ami-audio", "video:x86_64": "ami-video", "restore:x86_64": "ami-restore", "ollama:x86_64": "ami-ollama"},
		Host: hostOf(map[string]string{"image": "h1", "audio": "h2", "video": "h3", "restore": "h4"})}
	got := map[string]ImageStatus{}
	for _, s := range dc.Check(context.Background()) {
		got[s.Family] = s
	}
	if len(got) != 4 {
		t.Fatalf("ollama has no path list and must be skipped: %v", got)
	}
	if got["image"].Stale || got["image"].Reason != "" {
		t.Errorf("image should match: %+v", got["image"])
	}
	if !got["audio"].Stale || got["audio"].Reason != "manifest differs" {
		t.Errorf("audio should be stale: %+v", got["audio"])
	}
	if got["video"].Stale || got["video"].Reason != "ami has no runtime-manifest tag" {
		t.Errorf("an untagged image is unknown, not stale: %+v", got["video"])
	}
	if got["restore"].Stale || got["restore"].Reason != "ami not found" {
		t.Errorf("a missing ami is unknown, not stale: %+v", got["restore"])
	}
	if !dc.Stale("audio") || dc.Stale("image") || dc.Stale("nothing") {
		t.Error("Stale() disagrees with the pass")
	}
}

func TestDriftNeverClaimsStaleWithoutEvidence(t *testing.T) {
	api := &fakeImages{err: errors.New("throttled")}
	dc := &DriftChecker{API: api, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		AMIs: map[string]string{"image:x86_64": "ami-1"}, Host: hostOf(map[string]string{"image": "h"})}
	if s := dc.Check(context.Background())[0]; s.Stale || s.Reason != "describe images failed" {
		t.Errorf("%+v", s)
	}
	dc = &DriftChecker{API: &fakeImages{tags: map[string]map[string]string{"ami-1": {TagRuntimeManifest: "x"}}}, Log: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		AMIs: map[string]string{"image:x86_64": "ami-1"}, Host: hostOf(nil)}
	if s := dc.Check(context.Background())[0]; s.Stale || !strings.HasPrefix(s.Reason, "host manifest failed") {
		t.Errorf("%+v", s)
	}
}

func TestDriftLogsOncePerDay(t *testing.T) {
	var buf bytes.Buffer
	clock := newClock(time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC))
	api := &fakeImages{tags: map[string]map[string]string{"ami-1": {TagRuntimeManifest: "OLD"}}}
	dc := &DriftChecker{API: api, Log: slog.New(slog.NewTextHandler(&buf, nil)), Now: clock.Now,
		AMIs: map[string]string{"restore:x86_64": "ami-1"}, Host: hostOf(map[string]string{"restore": "NEW"})}
	want := "burst: image restore is stale vs host (rebuild with scripts/burst/build-image.sh restore)"
	dc.Check(context.Background())
	dc.Check(context.Background())
	clock.Advance(6 * time.Hour)
	dc.Check(context.Background())
	if n := strings.Count(buf.String(), want); n != 1 {
		t.Fatalf("expected 1 log line within a day, got %d:\n%s", n, buf.String())
	}
	clock.Advance(19 * time.Hour)
	dc.Check(context.Background())
	if n := strings.Count(buf.String(), want); n != 2 {
		t.Fatalf("expected a second line after 24 h, got %d", n)
	}
	// Rebuilt: tag now matches -> flag clears.
	api.tags["ami-1"][TagRuntimeManifest] = "NEW"
	dc.Check(context.Background())
	if dc.Stale("restore") {
		t.Error("flag should clear after a rebuild")
	}
}

func TestStaleImagesStillRunUnlessRefused(t *testing.T) {
	cfg := testCfg()
	r := newRig(cfg)
	r.p.Stale = func(string) bool { return true }
	if _, err := r.p.Launch(context.Background(), Offer{JobID: "j", Kind: "remove_background", Tier: "operator"}); err != nil {
		t.Fatalf("a stale image must still run by default: %v", err)
	}
	cfg.RefuseStaleImages = true
	r = newRig(cfg)
	r.p.Stale = func(f string) bool { return f == "image" }
	_, err := r.p.Launch(context.Background(), Offer{JobID: "j2", Kind: "remove_background", Tier: "operator"})
	if !errors.Is(err, ErrStaleImage) {
		t.Fatalf("want ErrStaleImage, got %v", err)
	}
	if _, err := r.p.Launch(context.Background(), Offer{JobID: "j3", Kind: "demucs", Tier: "operator"}); err != nil {
		t.Fatalf("a fresh family must still launch: %v", err)
	}
}

func TestReaperSparesBuildersUntilTheyAreFourHoursOld(t *testing.T) {
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	prov := &fakeProvider{tagged: []Instance{
		{ID: "i-new", Type: "g6.xlarge", State: "running", Build: "restore", LaunchTime: now.Add(-2 * time.Hour)},
		{ID: "i-old", Type: "g6.xlarge", State: "running", Build: "restore", LaunchTime: now.Add(-5 * time.Hour)},
	}}
	r := &Reaper{Cfg: config.BurstConfig{MaxInstanceMinutes: 120}, Provider: prov, Now: func() time.Time { return now }}
	rep := r.Pass(context.Background(), false, false)
	if len(rep.Terminated) != 1 || rep.Terminated[0] != "i-old" {
		t.Fatalf("only the runaway builder should be reaped: %+v", rep)
	}
}

// MM139: a fresh-install family (lab-voice) has no host runtime. It is never
// stale, the checker does not call the host manifest for it, and the status says why.
func TestDriftFreshFamilyIsNeverStale(t *testing.T) {
	api := &fakeImages{tags: map[string]map[string]string{
		"ami-lab":   {TagRuntimeManifest: "recorded-sha"},
		"ami-image": {TagRuntimeManifest: "h1"},
	}}
	host := func(f string) (manifest.Manifest, error) {
		if f == "lab-voice" {
			t.Fatal("the host manifest must not be computed for a fresh family")
		}
		return manifest.Manifest{Family: f, SHA256: "h1"}, nil
	}
	dc := &DriftChecker{API: api, AMIs: map[string]string{"lab-voice:x86_64": "ami-lab", "image:x86_64": "ami-image"}, Host: host}
	got := map[string]ImageStatus{}
	for _, s := range dc.Check(context.Background()) {
		got[s.Family] = s
	}
	lab := got["lab-voice"]
	if lab.Stale || !lab.Found || lab.ImageManifest != "recorded-sha" || !strings.Contains(lab.Reason, "fresh install") {
		t.Fatalf("lab-voice: %+v", lab)
	}
	if got["image"].Stale || dc.Stale("lab-voice") {
		t.Fatalf("image %+v", got["image"])
	}
	// An untagged or missing fresh AMI is still reported.
	dc2 := &DriftChecker{API: &fakeImages{tags: map[string]map[string]string{"ami-lab": {}}}, AMIs: map[string]string{"lab-voice:x86_64": "ami-lab"}, Host: host}
	if s := dc2.Check(context.Background())[0]; s.Reason != "ami has no runtime-manifest tag" || s.Stale {
		t.Fatalf("untagged: %+v", s)
	}
}
