package services

import (
	"reflect"
	"testing"
)

var wantLimits = []string{"-limit", "memory", "512MiB", "-limit", "map", "1GiB", "-limit", "disk", "2GiB"}

func TestBuildStudioImageProxyArgsOpaque(t *testing.T) {
	got := buildStudioImageProxyArgs("/in/a.heic", "/w/proxy.jpg", false)
	want := append(append([]string{}, wantLimits...),
		"/in/a.heic[0]", "-auto-orient", "-colorspace", "sRGB", "-resize", "1920x1920>", "-strip",
		"-quality", "92", "/w/proxy.jpg")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestBuildStudioImageProxyArgsAlpha(t *testing.T) {
	got := buildStudioImageProxyArgs("/in/a.png", "/w/proxy.png", true)
	want := append(append([]string{}, wantLimits...),
		"/in/a.png[0]", "-auto-orient", "-colorspace", "sRGB", "-resize", "1920x1920>", "-strip", "/w/proxy.png")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %q\nwant %q", got, want)
	}
}

func TestBuildStudioImageThumbArgs(t *testing.T) {
	got := buildStudioImageThumbArgs("/in/a.gif", "/w/thumb.jpg", false)
	want := append(append([]string{}, wantLimits...),
		"/in/a.gif[0]", "-auto-orient", "-colorspace", "sRGB", "-resize", "320x320>", "-strip",
		"-quality", "92", "/w/thumb.jpg")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("opaque got %q\nwant %q", got, want)
	}
	got = buildStudioImageThumbArgs("/in/a.png", "/w/thumb.png", true)
	want = append(append([]string{}, wantLimits...),
		"/in/a.png[0]", "-auto-orient", "-colorspace", "sRGB", "-resize", "320x320>", "-strip", "/w/thumb.png")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("alpha got %q\nwant %q", got, want)
	}
}

func TestBuildStudioSVGArgs(t *testing.T) {
	if got, want := buildStudioSVGRsvgArgs("/i.svg", "/o.png"), []string{"-w", "1920", "-h", "1920", "-a", "-f", "png", "-o", "/o.png", "/i.svg"}; !reflect.DeepEqual(got, want) {
		t.Fatalf("rsvg got %q", got)
	}
	got := buildStudioSVGMagickArgs("/i.svg", "/o.png")
	want := append(append([]string{}, wantLimits...), "-density", "192", "-background", "none", "/i.svg[0]", "/o.png")
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("magick svg got %q", got)
	}
}

func TestStudioImageAlphaHelpers(t *testing.T) {
	for in, want := range map[string]bool{"True": true, "Blend": true, "Undefined": false, "False": false, "": false, "Off": false} {
		if got := studioAlphaFromIdentify(in); got != want {
			t.Errorf("%q -> %v want %v", in, got, want)
		}
	}
	for ext, want := range map[string]bool{".png": true, "webp": true, ".JPG": false, "heic": false, "svg": true} {
		if got := studioExtMayHaveAlpha(ext); got != want {
			t.Errorf("%q -> %v want %v", ext, got, want)
		}
	}
	if studioImageExt(true) != "png" || studioImageExt(false) != "jpg" {
		t.Error("ext mapping")
	}
}
