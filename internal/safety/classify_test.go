package safety

import "testing"

func TestClassify_Critical(t *testing.T) {
	in := &ScanInput{
		TOSCategories: []any{"csam"},
	}
	c := Classify(in)
	if !c.ShouldCreateIncident || c.Severity != "critical" {
		t.Errorf("expected critical incident, got %+v", c)
	}
}

func TestClassify_High_Unsafe(t *testing.T) {
	in := &ScanInput{SafetyRating: "unsafe"}
	c := Classify(in)
	if !c.ShouldCreateIncident || c.Severity != "high" {
		t.Errorf("expected high incident, got %+v", c)
	}
}

func TestClassify_High_HarmfulContent(t *testing.T) {
	in := &ScanInput{HarmfulContent: true}
	c := Classify(in)
	if !c.ShouldCreateIncident || c.Severity != "high" {
		t.Errorf("expected high incident, got %+v", c)
	}
}

func TestClassify_Medium(t *testing.T) {
	in := &ScanInput{SafetyRating: "moderate", Labels: []any{"weapon"}}
	c := Classify(in)
	if !c.ShouldCreateIncident || c.Severity != "medium" {
		t.Errorf("expected medium incident, got %+v", c)
	}
}

func TestClassify_Safe(t *testing.T) {
	in := &ScanInput{SafetyRating: "safe"}
	c := Classify(in)
	if c.ShouldCreateIncident {
		t.Errorf("expected no incident, got %+v", c)
	}
}
