package handlers

import (
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
	"github.com/mrrobotisreal/media_manipulator_api/internal/retention"
)

const dAsset = "dddddddd-0000-0000-0000-000000000001"

func sp(s string) *string { return &s }

func TestSanitizeAssetPatch(t *testing.T) {
	p, ok := sanitizeAssetPatch(StudioAssetPatchRequest{
		DisplayName: sp("  Intro \n\t take   2\x00 "),
		Folder:      sp("B-roll/Day 1"),
		Label:       sp(" RED "),
	})
	if !ok {
		t.Fatal("valid patch rejected")
	}
	if *p.DisplayName != "Intro take 2" {
		t.Errorf("display name %q", *p.DisplayName)
	}
	if *p.Folder != "B-roll Day 1" {
		t.Errorf("folder %q", *p.Folder)
	}
	if *p.Label != "red" {
		t.Errorf("label %q", *p.Label)
	}

	if _, ok := sanitizeAssetPatch(StudioAssetPatchRequest{Label: sp("chartreuse")}); ok {
		t.Error("label outside the palette must be rejected")
	}
	// Empty string clears; absent stays nil.
	p, ok = sanitizeAssetPatch(StudioAssetPatchRequest{Label: sp(""), Folder: sp("")})
	if !ok || *p.Label != "" || *p.Folder != "" || p.DisplayName != nil {
		t.Errorf("clear/absent mapping wrong: %+v", p)
	}
	long := strings.Repeat("é", 500)
	p, _ = sanitizeAssetPatch(StudioAssetPatchRequest{DisplayName: &long, Folder: &long})
	if n := len([]rune(*p.DisplayName)); n != studioMaxDisplayNameRunes {
		t.Errorf("display name runes %d", n)
	}
	if n := len([]rune(*p.Folder)); n != studioMaxFolderRunes {
		t.Errorf("folder runes %d", n)
	}
}

func TestDeleteAssetOwnershipAndShape(t *testing.T) {
	cases := []matrixCase{
		{"owner", owner1, signed, 200},
		{"other user", owner2, signed, 404},
		{"other session", nil, map[string]string{"X-MM-Session-ID": "sess-2"}, 404},
		{"guest session on a user-owned project", nil, map[string]string{"X-MM-Session-ID": "sess-1"}, 404},
		{"no auth", nil, nil, 401},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeData{assetProj: dProj, assetFound: true, projUser: "user-1", projSess: "sess-1", projFound: true,
				expire: retention.ExpireResult{Matched: 1, Objects: 1, Bytes: 10}}
			r, _ := dataRouter(f, tc.user)
			w := call(r, "DELETE", "/studio/assets/"+dAsset, "", tc.headers)
			if w.Code != tc.want {
				t.Fatalf("code %d want %d: %s", w.Code, tc.want, w.Body.String())
			}
			if tc.want == 200 {
				if len(f.filters) != 1 || f.filters[0].AssetID != dAsset || f.filters[0].ProjectID != "" {
					t.Errorf("must sweep exactly the asset's ledger row: %+v", f.filters)
				}
			} else if len(f.filters) != 0 || len(f.assetRowDel) != 0 {
				t.Errorf("a refused delete must touch nothing: %+v %v", f.filters, f.assetRowDel)
			}
		})
	}
	t.Run("unknown asset", func(t *testing.T) {
		f := &fakeData{}
		r, _ := dataRouter(f, owner1)
		if w := call(r, "DELETE", "/studio/assets/"+dAsset, "", signed); w.Code != 404 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("guest owner", func(t *testing.T) {
		f := &fakeData{assetProj: dProj, assetFound: true, projSess: "sess-1", projFound: true,
			expire: retention.ExpireResult{Matched: 1}}
		r, _ := dataRouter(f, nil)
		if w := call(r, "DELETE", "/studio/assets/"+dAsset, "", map[string]string{"X-MM-Session-ID": "sess-1"}); w.Code != 200 {
			t.Fatalf("code %d", w.Code)
		}
	})
	t.Run("pre-ledger asset falls back to row delete", func(t *testing.T) {
		f := &fakeData{assetProj: dProj, assetFound: true, projUser: "user-1", projFound: true, assetKeys: nil}
		r, _ := dataRouter(f, owner1)
		if w := call(r, "DELETE", "/studio/assets/"+dAsset, "", signed); w.Code != 200 {
			t.Fatalf("code %d", w.Code)
		}
	})
}

func TestStudioEffectiveKind(t *testing.T) {
	audioOnly := &models.VideoProbeResponse{HasAudio: true, AudioCodec: "opus"}
	if got := studioEffectiveKind(models.StudioMediaKindVideo, audioOnly); got != models.StudioMediaKindAudio {
		t.Errorf("audio-only webm should be audio, got %s", got)
	}
	withVideo := &models.VideoProbeResponse{HasAudio: true, VideoCodec: "vp9", Width: 640, Height: 480}
	if got := studioEffectiveKind(models.StudioMediaKindVideo, withVideo); got != models.StudioMediaKindVideo {
		t.Errorf("real video must stay video, got %s", got)
	}
	if got := studioEffectiveKind(models.StudioMediaKindVideo, &models.VideoProbeResponse{}); got != models.StudioMediaKindVideo {
		t.Errorf("failed probe keeps sniffed kind, got %s", got)
	}
	if got := studioEffectiveKind(models.StudioMediaKindImage, audioOnly); got != models.StudioMediaKindImage {
		t.Error("image kind untouched")
	}
}

func TestVoiceoverUploadsPresign(t *testing.T) {
	for _, body := range []string{
		`{"fileName":"Voiceover 1.webm","contentType":"audio/webm;codecs=opus","fileSizeBytes":1000}`,
		`{"fileName":"Voiceover 2.m4a","contentType":"audio/mp4","fileSizeBytes":1000}`,
	} {
		if w := doPresign(t, body); w.Code != 201 {
			t.Errorf("%s -> %d %s", body, w.Code, w.Body.String())
		}
	}
}
