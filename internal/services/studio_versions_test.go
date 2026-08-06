package services

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/mrrobotisreal/media_manipulator_api/internal/models"
)

// Part 09 version history. As with part 07, the SQL runs only on the server —
// what is unit-testable here is the semantics encoded in the statements (the
// part a careless refactor would silently break) and the sidecar parsing the
// restore path depends on.

// The checkpoint must be one INSERT … SELECT against the live project row, so
// it can never interleave with a concurrent save and snapshot a torn document.
func TestStudioCreateVersionSnapshotsInOneStatement(t *testing.T) {
	if !strings.Contains(studioCreateVersionSQL, "INSERT INTO studio_project_versions") {
		t.Error("create must insert into studio_project_versions")
	}
	if !strings.Contains(studioCreateVersionSQL, "FROM studio_projects WHERE id = $1") {
		t.Error("create must SELECT the snapshot from the live studio_projects row")
	}
	for _, col := range []string{"revision", "tracks", "captions"} {
		if !strings.Contains(studioCreateVersionSQL, col) {
			t.Errorf("create must snapshot %s", col)
		}
	}
	if !strings.Contains(studioCreateVersionSQL, "RETURNING "+studioVersionMetaColumns) {
		t.Error("create must RETURNING the metadata columns")
	}
}

// Pruning may only ever touch 'auto' rows — manual versions are kept
// indefinitely — and must keep the newest N.
func TestStudioPruneOnlyAutosNewestFirst(t *testing.T) {
	if !strings.Contains(studioPruneAutoVersionsSQL, "DELETE FROM studio_project_versions") {
		t.Fatal("prune must delete from studio_project_versions")
	}
	// Both the delete predicate and the keep-list subquery must be scoped to
	// kind = 'auto'; two occurrences prove neither side can reach a manual.
	if strings.Count(studioPruneAutoVersionsSQL, "kind = 'auto'") != 2 {
		t.Error("prune must restrict BOTH the delete and the keep subquery to kind = 'auto'")
	}
	if strings.Contains(studioPruneAutoVersionsSQL, "'manual'") {
		t.Error("prune must never mention manual versions")
	}
	if !strings.Contains(studioPruneAutoVersionsSQL, "ORDER BY created_at DESC, id DESC") ||
		!strings.Contains(studioPruneAutoVersionsSQL, "LIMIT $2") {
		t.Error("prune must keep the newest $2 autos")
	}
}

// The list is metadata-only: shipping every snapshot body to the client would
// be hundreds of KB per row for a real timeline.
func TestStudioListVersionsHasNoBodies(t *testing.T) {
	if strings.Contains(studioVersionMetaColumns, "tracks") || strings.Contains(studioVersionMetaColumns, "captions") {
		t.Fatal("version metadata columns must not include the snapshot bodies")
	}
	if !strings.Contains(studioListVersionsSQL, studioVersionMetaColumns) {
		t.Error("list must select exactly the metadata columns")
	}
	if !strings.Contains(studioListVersionsSQL, "ORDER BY created_at DESC") {
		t.Error("list must be newest first")
	}
}

// GetVersion is scoped by project id, so a versionId from another project is
// a plain miss even after the caller passed their own project's ownership
// gate.
func TestStudioGetVersionScopedByProject(t *testing.T) {
	if !strings.Contains(studioGetVersionSQL, "project_id = $1 AND id = $2") {
		t.Error("get must predicate on BOTH project_id and version id")
	}
}

// parseVersionBodies maps the captions sidecar back into the exact fields
// SaveProject accepts — the restore round-trip depends on this staying in
// lockstep with studioProjectSidecar.
func TestParseVersionBodiesRoundTripsSidecar(t *testing.T) {
	enabled := false
	sidecar := studioProjectSidecar{
		SchemaVersion: models.StudioSchemaVersionCurrent,
		Cues:          []models.StudioCaptionCue{{ID: "c1", StartSeconds: 0, EndSeconds: 2, Text: "hi"}},
		Style:         &models.StudioCaptionStyle{FontSizePct: 5},
		Enabled:       &enabled,
		Audio:         &models.StudioAudioConfig{DuckingEnabled: true, DuckVoiceTrackID: "t1"},
		Markers:       []models.StudioMarker{{ID: "m1", T: 1.5}},
	}
	captions, err := json.Marshal(sidecar)
	if err != nil {
		t.Fatalf("marshal sidecar: %v", err)
	}
	tracks, err := json.Marshal([]models.StudioTrack{{ID: "v1", Kind: models.StudioTrackKindVideo}})
	if err != nil {
		t.Fatalf("marshal tracks: %v", err)
	}

	var snap StudioVersionSnapshot
	if err := parseVersionBodies(&snap, tracks, captions); err != nil {
		t.Fatalf("parseVersionBodies: %v", err)
	}
	if len(snap.Tracks) != 1 || snap.Tracks[0].ID != "v1" {
		t.Errorf("tracks lost: %+v", snap.Tracks)
	}
	if len(snap.Captions) != 1 || snap.Captions[0].Text != "hi" {
		t.Errorf("cues lost: %+v", snap.Captions)
	}
	if snap.CaptionStyle == nil || snap.CaptionStyle.FontSizePct != 5 {
		t.Errorf("style lost: %+v", snap.CaptionStyle)
	}
	if snap.CaptionsEnabled == nil || *snap.CaptionsEnabled {
		t.Error("enabled=false must survive the round trip")
	}
	if snap.Audio == nil || !snap.Audio.DuckingEnabled || snap.Audio.DuckVoiceTrackID != "t1" {
		t.Errorf("audio config lost: %+v", snap.Audio)
	}
	if len(snap.Markers) != 1 || snap.Markers[0].ID != "m1" {
		t.Errorf("markers lost: %+v", snap.Markers)
	}
}

// A pre-v2 project has NULL/empty snapshot columns; parsing must yield the
// same defaults scanProject uses (empty slices, nil pointers → SaveProject
// then applies its own captions-enabled default).
func TestParseVersionBodiesEmptyDefaults(t *testing.T) {
	var snap StudioVersionSnapshot
	if err := parseVersionBodies(&snap, nil, nil); err != nil {
		t.Fatalf("parseVersionBodies(nil): %v", err)
	}
	if snap.Tracks == nil || len(snap.Tracks) != 0 {
		t.Error("empty tracks must parse to an empty (non-nil) slice")
	}
	if snap.Captions == nil || len(snap.Captions) != 0 {
		t.Error("empty captions must parse to an empty (non-nil) slice")
	}
	if snap.CaptionsEnabled != nil {
		t.Error("absent enabled flag must stay nil (SaveProject defaults it to true)")
	}
}
