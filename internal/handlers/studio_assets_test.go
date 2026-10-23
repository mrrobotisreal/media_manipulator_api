package handlers

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

func TestIsSupportedStudioExtension(t *testing.T) {
	in := []string{"mp4", "mov", "webm", "mp3", "wav", "cube",
		"jpg", "jpeg", "png", "webp", "gif", "bmp", "tif", "tiff", "heic", "heif", "avif", "svg"}
	for _, e := range in {
		if !isSupportedStudioExtension(e) {
			t.Errorf("%q should be supported", e)
		}
	}
	for _, e := range []string{"exe", "pdf", "txt", "", "raw", "psd"} {
		if isSupportedStudioExtension(e) {
			t.Errorf("%q should be rejected", e)
		}
	}
}

func newPresignTestHandler() *StudioHandler {
	client := s3.New(s3.Options{
		Region:      "us-east-1",
		Credentials: aws.NewCredentialsCache(credentials.NewStaticCredentialsProvider("t", "t", "")),
	})
	return &StudioHandler{
		cfg: &config.Config{
			S3Bucket:       "test-bucket",
			S3PresignTTL:   15 * time.Minute,
			MaxVideoUpload: 1 << 30,
		},
		s3Presign: s3.NewPresignClient(client),
	}
}

func doPresign(t *testing.T, body string) *httptest.ResponseRecorder {
	t.Helper()
	gin.SetMode(gin.TestMode)
	h := newPresignTestHandler()
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest(http.MethodPost, "/api/studio/assets/presign", bytes.NewBufferString(body))
	c.Request.Header.Set("Content-Type", "application/json")
	h.PresignAsset(c)
	return w
}

func TestPresignAssetImages(t *testing.T) {
	cases := []struct {
		name string
		body string
		want int
	}{
		{"jpg ok", `{"fileName":"photo.jpg","contentType":"image/jpeg","fileSizeBytes":1000}`, http.StatusCreated},
		{"png ok", `{"fileName":"a.png","contentType":"image/png","fileSizeBytes":1000}`, http.StatusCreated},
		{"heic empty type derived", `{"fileName":"x.heic","contentType":"","fileSizeBytes":1000}`, http.StatusCreated},
		{"avif octet-stream derived", `{"fileName":"x.avif","contentType":"application/octet-stream","fileSizeBytes":1000}`, http.StatusCreated},
		{"svg empty derived", `{"fileName":"x.svg","fileSizeBytes":1000}`, http.StatusCreated},
		{"jpg with video type rejected", `{"fileName":"x.jpg","contentType":"video/mp4","fileSizeBytes":1000}`, http.StatusBadRequest},
		{"exe rejected", `{"fileName":"x.exe","contentType":"application/octet-stream","fileSizeBytes":1000}`, http.StatusBadRequest},
		{"mp4 still ok", `{"fileName":"x.mp4","contentType":"video/mp4","fileSizeBytes":1000}`, http.StatusCreated},
		{"mp3 still ok", `{"fileName":"x.mp3","contentType":"audio/mpeg","fileSizeBytes":1000}`, http.StatusCreated},
		{"mp4 with image type rejected", `{"fileName":"x.mp4","contentType":"image/png","fileSizeBytes":1000}`, http.StatusBadRequest},
		{"cube ok", `{"fileName":"x.cube","contentType":"text/plain","fileSizeBytes":1000}`, http.StatusCreated},
		{"image too large", `{"fileName":"x.jpg","contentType":"image/jpeg","fileSizeBytes":2147483648}`, http.StatusRequestEntityTooLarge},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			w := doPresign(t, tc.body)
			if w.Code != tc.want {
				t.Fatalf("status = %d, want %d; body=%s", w.Code, tc.want, w.Body.String())
			}
		})
	}
}
