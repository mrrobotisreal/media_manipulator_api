package handlers

import (
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

func docJob(opts map[string]interface{}) *models.ConversionJob {
	return &models.ConversionJob{
		OriginalFile: models.OriginalFileInfo{Name: "report.pdf", Type: "application/pdf"},
		Options:      opts,
	}
}

func TestPDFOutputExtension(t *testing.T) {
	h := &ConversionHandler{}
	cases := []struct {
		name string
		opts map[string]interface{}
		want string
	}{
		{"default all -> zip", map[string]interface{}{"format": "jpg"}, ".zip"},
		{"all png -> zip", map[string]interface{}{"format": "png", "pageSelection": "all"}, ".zip"},
		{"first jpg -> jpg", map[string]interface{}{"format": "jpg", "pageSelection": "first"}, ".jpg"},
		{"first png -> png", map[string]interface{}{"format": "png", "pageSelection": "first"}, ".png"},
		{"jpeg alias -> jpg", map[string]interface{}{"format": "jpeg", "pageSelection": "first"}, ".jpg"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := h.getOutputExtension(docJob(tc.opts)); got != tc.want {
				t.Fatalf("getOutputExtension = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestPDFOutputFilename(t *testing.T) {
	h := &ConversionHandler{}
	if got := h.getOutputFilename(docJob(map[string]interface{}{"format": "jpg", "pageSelection": "all"})); got != "report_pages.zip" {
		t.Fatalf("all-pages filename = %q, want report_pages.zip", got)
	}
	if got := h.getOutputFilename(docJob(map[string]interface{}{"format": "png", "pageSelection": "first"})); got != "report_converted.png" {
		t.Fatalf("first-page filename = %q, want report_converted.png", got)
	}
}

func TestImageToPDFOutputExtension(t *testing.T) {
	h := &ConversionHandler{}
	job := &models.ConversionJob{
		OriginalFile: models.OriginalFileInfo{Name: "photo.jpg", Type: "image/jpeg"},
		Options:      map[string]interface{}{"format": "pdf"},
	}
	if got := h.getOutputExtension(job); got != ".pdf" {
		t.Fatalf("image->pdf extension = %q, want .pdf", got)
	}
	if got := h.getOutputFilename(job); got != "photo_converted.pdf" {
		t.Fatalf("image->pdf filename = %q, want photo_converted.pdf", got)
	}
}
