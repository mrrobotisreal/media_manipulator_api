package handlers

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/analyticsemit"
	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
)

// MM101: server_photo_export_counted from the export, stateless export and
// batch handlers, via the `counted` seam (as the Motion counter's tests).

type countedCall struct {
	format, tier, mp string
	batch            bool
}

func recordCounted(e *renderEnv) *[]countedCall {
	var calls []countedCall
	e.h.counted = func(_ *gin.Context, format, tier, mp string, batch bool) {
		calls = append(calls, countedCall{format, tier, mp, batch})
	}
	return &calls
}

func TestPhotoCounted_ProjectExportCarriesBuckets(t *testing.T) {
	e := newRenderEnv(t)
	calls := recordCounted(e)
	id := e.create("free")
	// 4000x3000 = 12 MP source, free cap 12 -> <=16.
	if w := e.postJSON("/api/photo/projects/"+id+"/export", "free", map[string]any{"format": "webp"}, nil); w.Code != http.StatusAccepted {
		t.Fatalf("export = %d %s", w.Code, w.Body.String())
	}
	// A smaller requested size clamps the bucket: 6 MP -> <=8.
	if w := e.postJSON("/api/photo/projects/"+id+"/export", "free", map[string]any{"format": "png", "maxMegapixels": 6}, nil); w.Code != http.StatusAccepted {
		t.Fatalf("export = %d", w.Code)
	}
	want := []countedCall{{"webp", "free", "<=16", false}, {"png", "free", "<=8", false}}
	if len(*calls) != 2 || (*calls)[0] != want[0] || (*calls)[1] != want[1] {
		t.Fatalf("counted = %+v, want %+v", *calls, want)
	}
}

func TestPhotoCounted_BatchIsOneEventAndRefusalsEmitNothing(t *testing.T) {
	e := newRenderEnv(t)
	calls := recordCounted(e)
	var ids []string
	for i := 0; i < 4; i++ {
		ids = append(ids, e.create("free"))
	}
	doc := map[string]any{"version": 1, "source": map[string]any{"width": 1, "height": 1}, "layers": []any{map[string]any{"id": "l", "type": "invert"}}}
	// Over the free cap (3): refused, nothing emitted.
	if w := e.postJSON("/api/photo/batch", "free", map[string]any{"projectIds": ids, "doc": doc, "format": "jpg"}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("over cap = %d", w.Code)
	}
	// A bad export format is refused: nothing emitted.
	if w := e.postJSON("/api/photo/projects/"+ids[0]+"/export", "free", map[string]any{"format": "gif"}, nil); w.Code != http.StatusBadRequest {
		t.Fatalf("bad format = %d", w.Code)
	}
	if len(*calls) != 0 {
		t.Fatalf("refusals must emit nothing, got %+v", *calls)
	}
	if w := e.postJSON("/api/photo/batch", "free", map[string]any{"projectIds": ids[:3], "doc": doc, "format": "png"}, nil); w.Code != http.StatusAccepted {
		t.Fatalf("batch = %d", w.Code)
	}
	if len(*calls) != 1 || (*calls)[0] != (countedCall{"png", "free", "<=16", true}) {
		t.Fatalf("a batch of 3 is ONE event, got %+v", *calls)
	}
}

func TestPhotoCounted_StatelessExportAnonymous(t *testing.T) {
	e := newRenderEnv(t)
	calls := recordCounted(e)
	body, ct := multipartFields(t, map[string][]byte{"file": jpegBytes}, map[string]string{"file": "a.jpg"}, map[string]string{"format": "png", "maxMegapixels": "6"})
	w := e.do("POST", "/api/photo/export", "", body, map[string]string{"Content-Type": ct, "X-MM-Session-ID": "s-anon"})
	if w.Code != http.StatusAccepted {
		t.Fatalf("stateless export = %d %s", w.Code, w.Body.String())
	}
	if len(*calls) != 1 || (*calls)[0].tier != "anonymous" || (*calls)[0].mp != "<=8" || (*calls)[0].batch {
		t.Fatalf("counted = %+v", *calls)
	}
}

func TestPhotoCounted_EventShapeAndNoEmitterNoPanic(t *testing.T) {
	gin.SetMode(gin.TestMode)
	w := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(w)
	c.Request = httptest.NewRequest("POST", "/api/photo/batch", nil)
	c.Request.Header.Set("X-MM-Visitor-ID", " 11111111-1111-4111-8111-111111111111 ")
	c.Request.Header.Set("X-MM-Session-ID", "22222222-2222-4222-8222-222222222222")
	at := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	ev := photoCountedEvent(c, "jpg", "premium", "<=32", true, at)
	if ev.EventName != analyticsemit.ServerPhotoExportCounted || analyticsemit.ServerPhotoExportCounted != "server_photo_export_counted" {
		t.Fatalf("name = %q", ev.EventName)
	}
	if ev.VisitorID != "11111111-1111-4111-8111-111111111111" || ev.SessionID != "22222222-2222-4222-8222-222222222222" || !ev.EventTS.Equal(at) {
		t.Fatalf("identity = %+v", ev)
	}
	p := ev.Properties
	if len(p) != 4 || p["format"] != "jpg" || p["tier"] != "premium" || p["megapixels_bucket"] != "<=32" || p["batch"] != true {
		t.Fatalf("properties = %v", p)
	}
	if ev.Context["tier"] != "premium" {
		t.Fatalf("context = %v", ev.Context)
	}
	// No identity headers: the event carries empty ids and Emit drops it.
	c.Request.Header.Del("X-MM-Visitor-ID")
	if ev := photoCountedEvent(c, "jpg", "free", "<=2", false, at); ev.VisitorID != "" {
		t.Fatalf("visitor = %q", ev.VisitorID)
	}
	// A nil emitter leaves the seam unset; an unset seam is a no-op.
	h := NewPhotoHandler(newFakePhotoStore(), config.Load(), &fakeIngester{}, quiet)
	h.SetAnalyticsEmitter(nil)
	if h.counted != nil {
		t.Fatal("a nil emitter must not install the hook")
	}
	h.emitCounted(c, "jpg", "<=2", false)
}

func TestMegapixelBucket(t *testing.T) {
	for in, want := range map[float64]string{0.5: "<=2", 2: "<=2", 2.1: "<=8", 8: "<=8", 12: "<=16", 16: "<=16", 24: "<=32", 32: "<=32", 33: ">32", 120: ">32"} {
		if got := analyticsemit.MegapixelBucket(in); got != want {
			t.Errorf("MegapixelBucket(%v) = %q, want %q", in, got, want)
		}
	}
}
