package handlers

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/tiers"
)

// GET /api/tiers: the retired batch flag is gone (no batch feature exists),
// exportsPerDay is present on every tier, the four public tiers are listed in
// ladder order (MM39) and the Pro purchase/price fields sit beside Premium's.
func TestTiersResponseShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	r := gin.New()
	r.GET("/api/tiers", h.Tiers)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/api/tiers", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d", w.Code)
	}
	if got := w.Header().Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("Cache-Control = %q", got)
	}
	var body struct {
		Tiers []map[string]any `json:"tiers"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Tiers) != 4 {
		t.Fatalf("want 4 public tiers, got %d", len(body.Tiers))
	}
	for i, want := range []string{"anonymous", "free", "premium", "pro"} {
		if body.Tiers[i]["tier"] != want {
			t.Errorf("tiers[%d] = %v, want %s", i, body.Tiers[i]["tier"], want)
		}
	}
	var top map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
		t.Fatal(err)
	}
	for _, k := range []string{"premiumPurchasable", "premiumPriceUSD", "premiumPriceYearlyUSD", "proPurchasable", "proPriceUSD", "proPriceYearlyUSD"} {
		if _, has := top[k]; !has {
			t.Errorf("missing top-level field %s", k)
		}
	}
	if _, isBool := top["proPurchasable"].(bool); !isBool {
		t.Errorf("proPurchasable must be a bool, got %T", top["proPurchasable"])
	}
	for _, d := range body.Tiers {
		limits, _ := d["limits"].(map[string]any)
		if limits == nil {
			// Limits may be embedded flat instead of nested.
			limits = d
		}
		if _, has := limits["batch"+"Enabled"]; has {
			t.Errorf("%v: retired batch flag must be absent", d["tier"])
		}
		if _, has := limits["exportsPerDay"]; !has {
			t.Errorf("%v: exportsPerDay must be present", d["tier"])
		}
		assertVectorShape(t, d["tier"], limits)
	}
}

// MM43: every public tier carries the Motion fields; vectorFormats is an
// array, vectorVideo a nested {maxHeight,maxFps,maxSeconds} object, and the
// tiers without raster video (anonymous, free) carry zeros there.
func assertVectorShape(t *testing.T, tier any, limits map[string]any) {
	t.Helper()
	for _, k := range []string{"vectorExportsPerDay", "vectorProjects", "vectorMaxDurationMs"} {
		if _, isNum := limits[k].(float64); !isNum {
			t.Errorf("%v: %s must be a number, got %T", tier, k, limits[k])
		}
	}
	formats, isArr := limits["vectorFormats"].([]any)
	if !isArr || len(formats) == 0 {
		t.Errorf("%v: vectorFormats must be a non-empty array, got %#v", tier, limits["vectorFormats"])
	}
	for _, f := range formats {
		if f == "svg_static" {
			t.Errorf("%v: svg_static is uncounted and must not be listed", tier)
		}
	}
	video, isObj := limits["vectorVideo"].(map[string]any)
	if !isObj {
		t.Fatalf("%v: vectorVideo must be an object, got %#v", tier, limits["vectorVideo"])
	}
	for _, k := range []string{"maxHeight", "maxFps", "maxSeconds"} {
		if _, isNum := video[k].(float64); !isNum {
			t.Errorf("%v: vectorVideo.%s must be a number", tier, k)
		}
	}
	if tier == "anonymous" || tier == "free" {
		if video["maxHeight"] != 0.0 || video["maxFps"] != 0.0 || video["maxSeconds"] != 0.0 {
			t.Errorf("%v: no raster video, so vectorVideo must be all zeros: %v", tier, video)
		}
	}
	switch tier {
	case "anonymous":
		if limits["vectorExportsPerDay"] != 2.0 || limits["vectorProjects"] != 1.0 || limits["vectorMaxDurationMs"] != 10000.0 {
			t.Errorf("anonymous vector row = %v", limits)
		}
	case "premium":
		if video["maxHeight"] != 720.0 || video["maxSeconds"] != 15.0 || limits["vectorExportsPerDay"] != 30.0 {
			t.Errorf("premium vector row = %v / %v", limits, video)
		}
	}
}
