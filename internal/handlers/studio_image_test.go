package handlers

import (
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func TestStudioKindForFileType(t *testing.T) {
	cases := map[models.FileType]models.StudioMediaKind{
		models.FileTypeVideo: models.StudioMediaKindVideo,
		models.FileTypeAudio: models.StudioMediaKindAudio,
		models.FileTypeImage: models.StudioMediaKindImage,
	}
	for ft, want := range cases {
		got, ok := studioKindForFileType(ft)
		if !ok || got != want {
			t.Errorf("%v -> %q,%v want %q", ft, got, ok, want)
		}
	}
	for _, ft := range []models.FileType{models.FileTypeDocument, models.FileTypeUnknown} {
		if _, ok := studioKindForFileType(ft); ok {
			t.Errorf("%v must be rejected", ft)
		}
	}
}

func TestStudioImageDimensions(t *testing.T) {
	cases := []struct {
		name    string
		in      map[string]any
		w, h, o int
	}{
		{"geometry plain", map[string]any{"Geometry": "4032x3024+0+0"}, 4032, 3024, 1},
		{"geometry + IM orientation name 6 swaps", map[string]any{"Geometry": "4032x3024+0+0", "Orientation": "RightTop"}, 3024, 4032, 6},
		{"exif numeric 8 swaps", map[string]any{"Geometry": "100x50+0+0", "exifTiff": map[string]any{"Orientation": float64(8)}}, 50, 100, 8},
		{"exiftool string rotate 90", map[string]any{"Geometry": "100x50+0+0", "exifTiff": map[string]any{"Orientation": "Rotate 90 CW"}}, 50, 100, 6},
		{"orientation 3 no swap", map[string]any{"Geometry": "100x50+0+0", "Orientation": "BottomRight"}, 100, 50, 3},
		{"orientation 5 swaps", map[string]any{"Geometry": "10x20+0+0", "exifTiff": map[string]any{"Orientation": "5"}}, 20, 10, 5},
		{"exif fallback", map[string]any{"container": map[string]any{"ImageWidth": float64(640), "ImageHeight": float64(480)}}, 640, 480, 1},
		{"exif Exif* fallback", map[string]any{"exifTiff": map[string]any{"ExifImageWidth": 800, "ExifImageHeight": 600}}, 800, 600, 1},
		{"garbage", map[string]any{"Geometry": "nonsense"}, 0, 0, 1},
		{"nil", nil, 0, 0, 1},
		{"empty", map[string]any{}, 0, 0, 1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w, h, o := studioImageDimensions(tc.in)
			if w != tc.w || h != tc.h || o != tc.o {
				t.Fatalf("got %d,%d,%d want %d,%d,%d", w, h, o, tc.w, tc.h, tc.o)
			}
		})
	}
}

func TestStudioDerivativeContentType(t *testing.T) {
	img := &models.StudioAsset{MediaKind: models.StudioMediaKindImage}
	vid := &models.StudioAsset{MediaKind: models.StudioMediaKindVideo}
	aud := &models.StudioAsset{MediaKind: models.StudioMediaKindAudio}
	cases := []struct {
		a          *models.StudioAsset
		which, key string
		want       string
	}{
		{img, "proxy", "studio/d/s/id_proxy.jpg", "image/jpeg"},
		{img, "proxy", "studio/d/s/id_proxy.png", "image/png"},
		{img, "proxy", "", "image/jpeg"},
		{img, "sprite", "studio/d/s/id_thumb.jpg", "image/jpeg"},
		{img, "sprite", "studio/d/s/id_thumb.png", "image/png"},
		{img, "file", "studio/d/s/x.heic", "image/heic"},
		{img, "file", "studio/d/s/x.jpeg", "image/jpeg"},
		{vid, "proxy", "k_proxy.mp4", "video/mp4"},
		{aud, "proxy", "k_proxy.m4a", "audio/mp4"},
		{vid, "sprite", "k_sprite.jpg", "image/jpeg"},
		{vid, "file", "k.mp4", "application/octet-stream"},
	}
	for _, tc := range cases {
		if got := studioDerivativeContentType(tc.a, tc.which, tc.key); got != tc.want {
			t.Errorf("%s %s -> %q want %q", tc.which, tc.key, got, tc.want)
		}
	}
}
