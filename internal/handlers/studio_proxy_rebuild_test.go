package handlers

import (
	"context"
	"errors"
	"net/http"
	"reflect"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

const (
	rbOld         = "studio/d/s/a_proxy.mp4"
	rbNew         = "studio/d/s/a_proxy_g1.mp4"
	rbOldReversed = "studio/d/s/a_proxy_reversed.mp4"
)

// fakeRebuild records the order of the rebuild's I/O steps.
type fakeRebuild struct {
	log       []string
	encodeErr error
	swapErr   error
	swapped   bool
	reversed  string
	retired   [][]string
}

func (f *fakeRebuild) steps() proxyRebuildSteps {
	return proxyRebuildSteps{
		encode: func(_ context.Context, newKey string) error {
			f.log = append(f.log, "encode:"+newKey)
			return f.encodeErr
		},
		swap: func(_ context.Context, oldKey, newKey string) (string, bool, error) {
			f.log = append(f.log, "swap:"+oldKey+"->"+newKey)
			return f.reversed, f.swapped, f.swapErr
		},
		retire: func(_ context.Context, keys []string) {
			f.log = append(f.log, "retire")
			f.retired = append(f.retired, keys)
		},
	}
}

// The ticket's acceptance: the new key differs from the old, the DB swap
// (which also clears the reversed proxy) happens BEFORE the old key is
// retired, and the old reversed proxy goes with it.
func TestRunProxyRebuildSwapsBeforeRetiring(t *testing.T) {
	f := &fakeRebuild{swapped: true, reversed: rbOldReversed}
	if err := runProxyRebuild(context.Background(), f.steps(), rbOld, rbNew); err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	want := []string{"encode:" + rbNew, "swap:" + rbOld + "->" + rbNew, "retire"}
	if !reflect.DeepEqual(f.log, want) {
		t.Fatalf("order = %v, want %v", f.log, want)
	}
	if !reflect.DeepEqual(f.retired, [][]string{{rbOld, rbOldReversed}}) {
		t.Errorf("retired %v, want the old proxy and old reversed proxy", f.retired)
	}
}

func TestRunProxyRebuildNoReversed(t *testing.T) {
	f := &fakeRebuild{swapped: true}
	if err := runProxyRebuild(context.Background(), f.steps(), rbOld, rbNew); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(f.retired, [][]string{{rbOld}}) {
		t.Errorf("retired %v", f.retired)
	}
}

// Failures never touch the old proxy: the asset keeps playing what it had, and
// the half-made new object is retired instead of orphaned.
func TestRunProxyRebuildFailuresKeepTheOldProxy(t *testing.T) {
	cases := []struct {
		name string
		f    *fakeRebuild
		want error
	}{
		{"encode fails", &fakeRebuild{encodeErr: errors.New("ffmpeg")}, nil},
		{"swap errors", &fakeRebuild{swapErr: errors.New("db down")}, nil},
		{"asset changed meanwhile", &fakeRebuild{swapped: false}, errProxyRebuildSuperseded},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := runProxyRebuild(context.Background(), tc.f.steps(), rbOld, rbNew)
			if err == nil {
				t.Fatal("want an error")
			}
			if tc.want != nil && !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
			if !reflect.DeepEqual(tc.f.retired, [][]string{{rbNew}}) {
				t.Errorf("retired %v, want only the new key", tc.f.retired)
			}
		})
	}
}

func TestRunProxyRebuildRefusesSameKey(t *testing.T) {
	f := &fakeRebuild{swapped: true}
	if err := runProxyRebuild(context.Background(), f.steps(), rbOld, rbOld); err == nil {
		t.Fatal("rebuilding onto the old key must be refused (it is cached for a week)")
	}
	if len(f.log) != 0 {
		t.Errorf("nothing may run: %v", f.log)
	}
}

func TestProxyRebuildEligibility(t *testing.T) {
	cases := []struct {
		name string
		a    models.StudioAsset
		want int
	}{
		{"video with proxy", models.StudioAsset{MediaKind: models.StudioMediaKindVideo, S3KeyProxy: rbOld}, 0},
		{"video still ingesting", models.StudioAsset{MediaKind: models.StudioMediaKindVideo}, http.StatusConflict},
		{"audio", models.StudioAsset{MediaKind: models.StudioMediaKindAudio, S3KeyProxy: "k.m4a"}, http.StatusBadRequest},
		{"image", models.StudioAsset{MediaKind: models.StudioMediaKindImage, S3KeyProxy: "k.jpg"}, http.StatusBadRequest},
		{"lut", models.StudioAsset{MediaKind: models.StudioMediaKindLUT}, http.StatusBadRequest},
	}
	for _, tc := range cases {
		if got, _ := proxyRebuildEligibility(&tc.a); got != tc.want {
			t.Errorf("%s: %d, want %d", tc.name, got, tc.want)
		}
	}
}

// Idempotent per asset: a second claim while one runs returns the running job.
func TestProxyRebuildRegistryDedupes(t *testing.T) {
	var r proxyRebuildRegistry
	first, ok := r.claim("a", proxyRebuildInFlight{jobID: "j1", generation: 1})
	if !ok || first.jobID != "j1" {
		t.Fatalf("first claim: %+v %v", first, ok)
	}
	again, ok := r.claim("a", proxyRebuildInFlight{jobID: "j2", generation: 1})
	if ok || again.jobID != "j1" {
		t.Fatalf("second claim must return the running job: %+v %v", again, ok)
	}
	if _, ok := r.claim("b", proxyRebuildInFlight{jobID: "j3"}); !ok {
		t.Fatal("another asset is independent")
	}
	r.release("a")
	if got, ok := r.claim("a", proxyRebuildInFlight{jobID: "j4"}); !ok || got.jobID != "j4" {
		t.Fatalf("after release a new rebuild may start: %+v %v", got, ok)
	}
}

// Owner-scoped like DeleteAsset (MM24 strict rule): a stranger gets 404 and
// nothing starts. The owner passes the gate (and then meets the missing S3
// client in this harness: 503).
func TestRebuildProxyOwnership(t *testing.T) {
	cases := []matrixCase{
		{"owner", owner1, signed, http.StatusServiceUnavailable},
		{"other user", owner2, signed, http.StatusNotFound},
		{"other session", nil, map[string]string{"X-MM-Session-ID": "sess-2"}, http.StatusNotFound},
		{"guest session on a user-owned project", nil, map[string]string{"X-MM-Session-ID": "sess-1"}, http.StatusNotFound},
		{"no auth", nil, nil, http.StatusUnauthorized},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeData{assetProj: dProj, assetFound: true, projUser: "user-1", projSess: "sess-1", projFound: true}
			r, _ := dataRouter(f, tc.user)
			w := call(r, "POST", "/studio/assets/"+dAsset+"/rebuild-proxy", "{}", tc.headers)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if len(f.filters) != 0 {
				t.Errorf("a gate refusal must retire nothing: %+v", f.filters)
			}
		})
	}
	t.Run("unknown asset", func(t *testing.T) {
		r, _ := dataRouter(&fakeData{}, owner1)
		if w := call(r, "POST", "/studio/assets/"+dAsset+"/rebuild-proxy", "{}", signed); w.Code != http.StatusNotFound {
			t.Fatalf("code %d", w.Code)
		}
	})
}

// retireStudioKeys expires each old key's own ledger row (S3Key filter), and
// never touches keys outside the Darkroom prefix.
func TestRetireStudioKeysUsesTheLedger(t *testing.T) {
	f := &fakeData{expire: retention.ExpireResult{Matched: 1, Objects: 1}}
	h := &StudioHandler{cfg: &config.Config{ContentStudioS3Prefix: "studio"}}
	h.SetDeletion(f, f)
	h.retireStudioKeys(context.Background(), []string{rbOld, "", "videos/x.mp4", "studio/../etc", rbOldReversed})
	want := []telemetry.LedgerFilter{{S3Key: rbOld}, {S3Key: rbOldReversed}}
	if !reflect.DeepEqual(f.filters, want) {
		t.Errorf("filters %+v, want %+v", f.filters, want)
	}
}
