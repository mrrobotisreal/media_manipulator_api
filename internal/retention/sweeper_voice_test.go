package retention

import (
	"context"
	"testing"
	"time"

	"github.com/mrrobotisreal/media_manipulator_api/internal/telemetry"
)

// MM77: a voice ledger row sweeps the mm_voices row first, then its reference
// and preview objects (named by the row AND by the s3_object rows registered
// with the voice id as parent), and stamps every one of those ledger rows.
func TestSweepVoiceRowThenObjects(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.AllowedPrefixes = append(e.cfg.AllowedPrefixes, "voices")
	ref := "voices/user-1/v-1/reference.wav"
	preview := "voices/user-1/v-1/preview.mp3"
	e.db.voices["v-1"] = []string{ref, preview}
	refRow := e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: ref, ParentLocator: "v-1", OwnerUserID: "user-1",
		ExpiresAt: sweepT0.Add(time.Hour)})
	prevRow := e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: preview, ParentLocator: "v-1", OwnerUserID: "user-1",
		ExpiresAt: sweepT0.Add(time.Hour)})
	row := e.db.add(telemetry.LedgerRow{Kind: KindVoice, Locator: "v-1", OwnerUserID: "user-1"})

	e.sweep()

	if _, still := e.db.voices["v-1"]; still {
		t.Error("the mm_voices row must be deleted")
	}
	if row.DeletedAt == nil {
		t.Fatal("voice ledger row should be done")
	}
	for _, k := range []string{ref, preview} {
		if !e.s3.gone[k] {
			t.Errorf("%s not deleted (calls %v)", k, e.s3.calls)
		}
	}
	if refRow.DeletedAt == nil || prevRow.DeletedAt == nil {
		t.Error("both object ledger rows must be stamped")
	}
}

// A retry after the row is already gone still finds the objects through the
// ledger children.
func TestSweepVoiceAfterRowGoneUsesLedgerChildren(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.AllowedPrefixes = append(e.cfg.AllowedPrefixes, "voices")
	ref := "voices/user-1/v-2/reference.wav"
	e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: ref, ParentLocator: "v-2", OwnerUserID: "user-1",
		ExpiresAt: sweepT0.Add(time.Hour)})
	row := e.db.add(telemetry.LedgerRow{Kind: KindVoice, Locator: "v-2", OwnerUserID: "user-1"})
	e.sweep()
	if row.DeletedAt == nil || !e.s3.gone[ref] {
		t.Fatalf("row %+v gone %v", row.DeletedAt, e.s3.gone)
	}
}

// voices/ must be on the sweeper's S3 allowlist, or the row stays retryable.
func TestSweepVoiceNeedsAllowedPrefix(t *testing.T) {
	e := newSweepEnv(t)
	e.db.voices["v-3"] = []string{"voices/user-1/v-3/reference.wav"}
	row := e.db.add(telemetry.LedgerRow{Kind: KindVoice, Locator: "v-3", OwnerUserID: "user-1"})
	e.sweep()
	if row.DeletedAt != nil || row.Attempts != 1 {
		t.Fatalf("a refused object delete must leave the voice row retryable: %+v", row)
	}
}

// MM77 acceptance 6 (cascade): deleting one voice retires exactly its ledger
// rows (row + reference + preview) and leaves another voice of the same user
// alone; account deletion (filter by user) retires every voice row and key.
func TestExpireVoiceCascade(t *testing.T) {
	e := newSweepEnv(t)
	e.cfg.AllowedPrefixes = append(e.cfg.AllowedPrefixes, "voices")
	mk := func(id string) []*fakeRow {
		ref, prev := "voices/user-1/"+id+"/reference.wav", "voices/user-1/"+id+"/preview.mp3"
		e.db.voices[id] = []string{ref, prev}
		far := sweepT0.Add(30 * 24 * time.Hour)
		return []*fakeRow{
			e.db.add(telemetry.LedgerRow{Kind: KindVoice, Locator: id, OwnerUserID: "user-1", ExpiresAt: far}),
			e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: ref, ParentLocator: id, OwnerUserID: "user-1", ExpiresAt: far}),
			e.db.add(telemetry.LedgerRow{Kind: KindS3Object, Locator: prev, ParentLocator: id, OwnerUserID: "user-1", ExpiresAt: far}),
		}
	}
	a, b := mk("v-a"), mk("v-b")
	sw := e.sweeper()

	res, err := sw.ExpireNow(context.Background(), telemetry.LedgerFilter{UserID: "user-1", VoiceID: "v-a"})
	if err != nil || res.Pending != 0 {
		t.Fatalf("voice delete: %+v %v", res, err)
	}
	for _, r := range a {
		if r.DeletedAt == nil {
			t.Errorf("voice a row %s %s must be retired", r.Kind, r.Locator)
		}
	}
	for _, r := range b {
		if r.DeletedAt != nil {
			t.Errorf("voice b row %s %s must be untouched", r.Kind, r.Locator)
		}
	}
	if _, still := e.db.voices["v-a"]; still {
		t.Error("voice a mm_voices row must be gone")
	}

	res, err = sw.ExpireNow(context.Background(), telemetry.LedgerFilter{UserID: "user-1"})
	if err != nil || res.Pending != 0 {
		t.Fatalf("account sweep: %+v %v", res, err)
	}
	for _, r := range b {
		if r.DeletedAt == nil {
			t.Errorf("account deletion must retire %s %s", r.Kind, r.Locator)
		}
	}
	for _, k := range []string{"voices/user-1/v-a/reference.wav", "voices/user-1/v-b/preview.mp3"} {
		if !e.s3.gone[k] {
			t.Errorf("%s must be deleted from S3", k)
		}
	}
}
