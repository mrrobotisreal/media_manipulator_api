package handlers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/mrrobotisreal/media_manipulator_api/internal/config"
	"github.com/mrrobotisreal/media_manipulator_api/internal/geo"
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
	// MM69: defaultCurrency is per caller, so no shared cache may store it.
	if got := w.Header().Get("Cache-Control"); got != "private, max-age=300" {
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
	// premiumPriceUSD, premiumPriceYearlyUSD, proPriceUSD, proPriceYearlyUSD are
	// DEPRECATED since MM69 (use prices.usd); kept for one release.
	for _, k := range []string{"premiumPurchasable", "premiumPriceUSD", "premiumPriceYearlyUSD", "proPurchasable", "proPriceUSD", "proPriceYearlyUSD", "defaultCurrency", "prices"} {
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
	case "pro":
		// MM49: Pro's own Motion row, the only public tier with raster video.
		if video["maxHeight"] != 2160.0 || video["maxFps"] != 60.0 || video["maxSeconds"] != 60.0 ||
			limits["vectorExportsPerDay"] != 150.0 || limits["vectorProjects"] != -1.0 {
			t.Errorf("pro vector row = %v / %v", limits, video)
		}
		has := map[string]bool{}
		for _, f := range formats {
			has[f.(string)] = true
		}
		for _, f := range []string{"png_sequence", "mp4", "webm", "gif", "svg_animated"} {
			if !has[f] {
				t.Errorf("pro vectorFormats %v lacks %s", formats, f)
			}
		}
	}
}

// MM69: /api/tiers serves both currency maps with identical numerals (equal to
// the deprecated *USD fields), and defaultCurrency from the caller's country:
// MaxMind first, then CF-IPCountry, else usd.
func TestTiersCurrencyShape(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := config.Load()
	get := func(h *AccountHandler, hdr map[string]string) map[string]any {
		r := gin.New()
		r.GET("/api/tiers", h.Tiers)
		req := httptest.NewRequest(http.MethodGet, "/api/tiers", nil)
		for k, v := range hdr {
			req.Header.Set(k, v)
		}
		w := httptest.NewRecorder()
		r.ServeHTTP(w, req)
		var top map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &top); err != nil {
			t.Fatal(err)
		}
		return top
	}
	h := NewAccountHandler(nil, tiers.New(cfg), cfg, nil, nil, nil)
	top := get(h, nil)
	if top["defaultCurrency"] != "usd" {
		t.Errorf("no country signal must default to usd, got %v", top["defaultCurrency"])
	}
	prices, _ := top["prices"].(map[string]any)
	usd, _ := prices["usd"].(map[string]any)
	eur, _ := prices["eur"].(map[string]any)
	if len(prices) != 2 || usd == nil || eur == nil {
		t.Fatalf("prices must have exactly usd and eur: %v", top["prices"])
	}
	want := map[string]any{"premiumMonthly": 8.0, "premiumYearly": 80.0, "proMonthly": 15.0, "proYearly": 150.0}
	for k, v := range want {
		if usd[k] != v || eur[k] != v {
			t.Errorf("%s: usd %v eur %v, want %v in both (same numerals)", k, usd[k], eur[k], v)
		}
	}
	for legacy, k := range map[string]string{"premiumPriceUSD": "premiumMonthly", "premiumPriceYearlyUSD": "premiumYearly", "proPriceUSD": "proMonthly", "proPriceYearlyUSD": "proYearly"} {
		if top[legacy] != usd[k] {
			t.Errorf("deprecated %s = %v, want prices.usd.%s %v", legacy, top[legacy], k, usd[k])
		}
	}
	for country, want := range map[string]string{"DE": "eur", "fr": "eur", "GB": "eur", "CH": "eur", "US": "usd", "XX": "usd", "T1": "usd", "": "usd"} {
		if got := get(h, map[string]string{"CF-IPCountry": country})["defaultCurrency"]; got != want {
			t.Errorf("CF-IPCountry %q: defaultCurrency %v, want %s", country, got, want)
		}
	}
	// MaxMind wins over the header.
	h.geo = fakeCountry{"IT"}
	if got := get(h, map[string]string{"CF-IPCountry": "US", "CF-Connecting-IP": "203.0.113.9"})["defaultCurrency"]; got != "eur" {
		t.Errorf("MaxMind IT must win over CF-IPCountry US, got %v", got)
	}
	h.geo = fakeCountry{""}
	if got := get(h, map[string]string{"CF-IPCountry": "DE", "CF-Connecting-IP": "203.0.113.9"})["defaultCurrency"]; got != "eur" {
		t.Errorf("an unknown MaxMind country must fall back to the header, got %v", got)
	}
	t.Logf("/api/tiers currency fields: defaultCurrency=%v prices=%v", top["defaultCurrency"], top["prices"])
}

type fakeCountry struct{ code string }

func (f fakeCountry) Lookup(context.Context, string) (*geo.Entry, error) {
	if f.code == "" {
		return nil, nil
	}
	return &geo.Entry{CountryCode: f.code}, nil
}
