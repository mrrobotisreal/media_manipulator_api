package burst

import (
	"context"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/ec2"
	"github.com/aws/aws-sdk-go-v2/service/ec2/types"

	"github.com/mrrobotisreal/media_manipulator_api/internal/burst/manifest"
)

// Image tag keys written by scripts/burst/build-image.sh (MM126).
const (
	TagImage           = "mm-burst-image"       // =1 on every AMI and snapshot the builder makes
	TagImageFamily     = "family"               // image|audio|video|restore|lab-voice
	TagRuntimeManifest = "runtime-manifest"     // sha256 of the family's host manifest at export time
	TagImageArch       = "arch"                 // ada+blackwell
	TagImageBuilt      = "mm-burst-image-built" // YYYY-MM-DD
)

// ImageAPI is the slice of *ec2.Client the drift check uses.
type ImageAPI interface {
	DescribeImages(ctx context.Context, in *ec2.DescribeImagesInput, opts ...func(*ec2.Options)) (*ec2.DescribeImagesOutput, error)
}

// ImageStatus is one family's image against the host runtime.
type ImageStatus struct {
	Family        string
	AMI           string // the configured BURST_AMIS id
	Found         bool   // DescribeImages returned it
	ImageManifest string // AMI tag runtime-manifest
	HostManifest  string // computed from the host paths now
	Stale         bool
	Reason        string // "", "manifest differs", "ami not found", "ami has no runtime-manifest tag", "host paths missing: ..."
	Built         string
}

// HostManifest computes a family's fingerprint on this machine (injected so
// tests need no filesystem).
type HostManifest func(family string) (manifest.Manifest, error)

// DriftChecker compares each configured family's AMI tag with the host.
type DriftChecker struct {
	API   ImageAPI
	AMIs  map[string]string // BURST_AMIS: "family:arch" -> ami id
	Host  HostManifest
	Log   *slog.Logger
	Now   func() time.Time
	Every time.Duration // recheck period (default 6 h); the log line is once a day

	mu       sync.Mutex
	last     map[string]ImageStatus
	lastLog  map[string]time.Time
	lastDone bool
}

func (d *DriftChecker) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

func (d *DriftChecker) log() *slog.Logger {
	if d.Log != nil {
		return d.Log
	}
	return slog.Default()
}

// families returns the configured families that have a path list, sorted, each
// with its AMI id (an "any"/x86_64 key is preferred; any arch key will do).
func (d *DriftChecker) families() map[string]string {
	out := map[string]string{}
	keys := make([]string, 0, len(d.AMIs))
	for k := range d.AMIs {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fam := k
		if i := strings.Index(k, ":"); i >= 0 {
			fam = k[:i]
		}
		known := manifest.IsFresh(fam)
		for _, f := range manifest.Families {
			known = known || f == fam
		}
		if !known {
			continue
		}
		if _, dup := out[fam]; !dup {
			out[fam] = d.AMIs[k]
		}
	}
	return out
}

// Check runs one pass. A failure to describe or to read the host is returned
// per family in Reason with Stale=false: drift is only claimed on evidence.
func (d *DriftChecker) Check(ctx context.Context) []ImageStatus {
	fams := d.families()
	names := make([]string, 0, len(fams))
	for f := range fams {
		names = append(names, f)
	}
	sort.Strings(names)
	var out []ImageStatus
	if d.API == nil || len(names) == 0 {
		return nil
	}
	ids := make([]string, 0, len(names))
	for _, f := range names {
		ids = append(ids, fams[f])
	}
	imgs := map[string]types.Image{}
	desc, err := d.API.DescribeImages(ctx, &ec2.DescribeImagesInput{ImageIds: ids, Owners: []string{"self"}})
	if err != nil {
		d.log().Warn("burst: could not describe worker images", "error", err.Error())
	} else {
		for _, im := range desc.Images {
			imgs[aws.ToString(im.ImageId)] = im
		}
	}
	for _, f := range names {
		st := ImageStatus{Family: f, AMI: fams[f]}
		im, ok := imgs[st.AMI]
		st.Found = ok
		for _, t := range im.Tags {
			switch aws.ToString(t.Key) {
			case TagRuntimeManifest:
				st.ImageManifest = aws.ToString(t.Value)
			case TagImageBuilt:
				st.Built = aws.ToString(t.Value)
			}
		}
		if manifest.IsFresh(f) {
			// MM139: installed fresh on the builder, nothing on the host to compare.
			// Never stale; the tag is the recorded-versions fingerprint.
			switch {
			case err != nil:
				st.Reason = "describe images failed"
			case !ok:
				st.Reason = "ami not found"
			case st.ImageManifest == "":
				st.Reason = "ami has no runtime-manifest tag"
			default:
				st.Reason = "fresh install (no host runtime to compare)"
			}
			out = append(out, st)
			continue
		}
		hm, herr := d.Host(f)
		switch {
		case herr != nil:
			st.Reason = "host manifest failed: " + herr.Error()
		case len(hm.MissingPaths()) > 0:
			st.HostManifest = hm.SHA256
			st.Reason = "host paths missing: " + strings.Join(hm.MissingPaths(), ", ")
		default:
			st.HostManifest = hm.SHA256
		}
		switch {
		case st.Reason != "":
		case err != nil:
			st.Reason = "describe images failed"
		case !ok:
			st.Reason = "ami not found"
		case st.ImageManifest == "":
			st.Reason = "ami has no runtime-manifest tag"
		case st.ImageManifest != st.HostManifest:
			st.Stale, st.Reason = true, "manifest differs"
		}
		out = append(out, st)
	}
	d.record(out)
	return out
}

// record stores the pass and logs each stale family at most once a day.
func (d *DriftChecker) record(sts []ImageStatus) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.last == nil {
		d.last, d.lastLog = map[string]ImageStatus{}, map[string]time.Time{}
	}
	now := d.now()
	for _, s := range sts {
		d.last[s.Family] = s
		if !s.Stale {
			delete(d.lastLog, s.Family)
			continue
		}
		if t, ok := d.lastLog[s.Family]; ok && now.Sub(t) < 24*time.Hour {
			continue
		}
		d.lastLog[s.Family] = now
		d.log().Warn(fmt.Sprintf("burst: image %s is stale vs host (rebuild with scripts/burst/build-image.sh %s)", s.Family, s.Family),
			"ami", s.AMI)
	}
	d.lastDone = true
}

// Stale reports the last pass's verdict for a family (false when never checked).
func (d *DriftChecker) Stale(family string) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.last[family].Stale
}

// Last returns the last pass sorted by family.
func (d *DriftChecker) Last() []ImageStatus {
	d.mu.Lock()
	defer d.mu.Unlock()
	out := make([]ImageStatus, 0, len(d.last))
	for _, s := range d.last {
		out = append(out, s)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Family < out[j].Family })
	return out
}

// Run checks once at boot (in the background, off the request path) and then
// every Every (default 6 h) until ctx ends.
func (d *DriftChecker) Run(ctx context.Context) {
	every := d.Every
	if every <= 0 {
		every = 6 * time.Hour
	}
	d.Check(ctx)
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			d.Check(ctx)
		}
	}
}
