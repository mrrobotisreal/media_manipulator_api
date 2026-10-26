package services

import (
	"strings"
	"testing"
)

const rbAsset = "aaaaaaaa-1111-2222-3333-444444444444"

// MM71: a proxy's generation is read from its key; anything that is not a
// generation key (the legacy name, foreign keys) is generation 0.
func TestStudioProxyGeneration(t *testing.T) {
	cases := []struct {
		key  string
		want int
	}{
		{"studio/20261001/s/" + rbAsset + "_proxy.mp4", 0},
		{"studio/20261001/s/" + rbAsset + "_proxy_g1.mp4", 1},
		{"studio/20261001/s/" + rbAsset + "_proxy_g12.mp4", 12},
		{"studio/20261001/s/" + rbAsset + "_proxy_g1_reversed.mp4", 0},
		{"studio/20261001/s/" + rbAsset + "_proxy_g.mp4", 0},
		{"studio/20261001/s/" + rbAsset + "_proxy_gx.mp4", 0},
		{"studio/20261001/s/other_proxy_g3.mp4", 0},
		{"", 0},
	}
	for _, tc := range cases {
		if got := StudioProxyGeneration(rbAsset, tc.key); got != tc.want {
			t.Errorf("StudioProxyGeneration(%q) = %d, want %d", tc.key, got, tc.want)
		}
	}
}

// A rebuild ALWAYS lands on a new key beside the original (proxies are cached
// for a week, so a key is never rewritten), one generation up.
func TestNextStudioVideoProxyKeyIsNewEveryTime(t *testing.T) {
	orig := "studio/20261001/sess/" + "src.mov"
	cur := "studio/20261001/sess/" + rbAsset + "_proxy.mp4"
	seen := map[string]bool{cur: true}
	for want := 1; want <= 3; want++ {
		next, gen := NextStudioVideoProxyKey(orig, rbAsset, cur)
		if gen != want {
			t.Fatalf("generation %d, want %d", gen, want)
		}
		if seen[next] {
			t.Fatalf("rebuild %d reused key %q", want, next)
		}
		if !strings.HasPrefix(next, "studio/20261001/sess/"+rbAsset+"_proxy_g") || !strings.HasSuffix(next, ".mp4") {
			t.Fatalf("key %q not beside the original", next)
		}
		seen[next] = true
		cur = next
	}
	// A new import is generation 1, so its first rebuild is 2.
	if got := studioVideoProxyKey("d", rbAsset, StudioProxyIngestGeneration); got != "d/"+rbAsset+"_proxy_g1.mp4" {
		t.Errorf("ingest key %q", got)
	}
	if got := studioVideoProxyKey("d", rbAsset, 0); got != "d/"+rbAsset+"_proxy.mp4" {
		t.Errorf("generation 0 must keep the legacy name, got %q", got)
	}
}

// The reversed proxy follows its source proxy's generation, so a reverse built
// after a rebuild never reuses (or is served from the cache of) the old key.
func TestStudioReversedProxyKeyFollowsGeneration(t *testing.T) {
	orig := "studio/d/s/src.mp4"
	if got := studioReversedProxyKey(orig, rbAsset, "studio/d/s/"+rbAsset+"_proxy.mp4", "mp4"); got != "studio/d/s/"+rbAsset+"_proxy_reversed.mp4" {
		t.Errorf("legacy reversed key changed: %q", got)
	}
	if got := studioReversedProxyKey(orig, rbAsset, "studio/d/s/"+rbAsset+"_proxy_g2.mp4", "mp4"); got != "studio/d/s/"+rbAsset+"_proxy_g2_reversed.mp4" {
		t.Errorf("generation reversed key: %q", got)
	}
	// Audio proxies are never rebuilt and keep the legacy reversed name.
	if got := studioReversedProxyKey(orig, rbAsset, "studio/d/s/"+rbAsset+"_proxy.m4a", "m4a"); got != "studio/d/s/"+rbAsset+"_proxy_reversed.m4a" {
		t.Errorf("audio reversed key: %q", got)
	}
}

// The rebuild encodes exactly like ingest: same argv builder, carrying MM70's
// videoProxyEncodeArgs (1 s keyframes, 2 Mb/s) and the proxy scale.
func TestBuildVideoProxyArgsUsesProxyEncode(t *testing.T) {
	for _, enc := range []string{"libx264", "h264_nvenc"} {
		args := strings.Join(buildVideoProxyArgs(enc, 720, "/in.mov", "/out.mp4"), " ")
		if !strings.Contains(args, strings.Join(videoProxyEncodeArgs(enc), " ")) {
			t.Errorf("%s: rebuild args lack videoProxyEncodeArgs: %s", enc, args)
		}
		for _, want := range []string{"-i /in.mov", "-vf scale=-2:720", "-c:a aac -b:a 128k", "-movflags +faststart /out.mp4", "-force_key_frames"} {
			if !strings.Contains(args, want) {
				t.Errorf("%s: missing %q in %s", enc, want, args)
			}
		}
	}
}

// The swap is one compare-and-set statement: it only moves a row that still
// holds the proxy the rebuild started from, points it at the new key, clears
// the reversed proxy (built from the old one) and leaves the sprite alone.
func TestStudioSwapProxySQL(t *testing.T) {
	sql := strings.Join(strings.Fields(studioSwapProxySQL), " ")
	for _, want := range []string{
		"WHERE id = $1 AND s3_key_proxy = $2",
		"FOR UPDATE",
		"SET s3_key_proxy = $3, s3_key_proxy_reversed = NULL",
		"RETURNING coalesce(prev.s3_key_proxy_reversed, '')",
	} {
		if !strings.Contains(sql, want) {
			t.Errorf("swap SQL lacks %q: %s", want, sql)
		}
	}
	if strings.Contains(sql, "thumbnail_sprite_url") {
		t.Errorf("the swap must not touch the filmstrip: %s", sql)
	}
}
