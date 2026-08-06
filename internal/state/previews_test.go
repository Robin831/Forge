package state

import (
	"testing"
	"time"
)

func samplePreview() Preview {
	return Preview{
		BeadID:       "Forge-ir70",
		Anvil:        "forge",
		Branch:       "forge/Forge-ir70",
		Status:       PreviewStarting,
		WorktreePath: "/anvil/.previews/Forge-ir70",
		Services: []PreviewService{
			{Name: "api", Port: 42001, Health: PreviewServiceStarting, PID: 1234, LogPath: "/logs/preview-api.log"},
			{Name: "client", Port: 42002, Health: PreviewServiceStarting, PID: 1235, Entry: true},
		},
	}
}

func TestUpsertPreviewRoundTrip(t *testing.T) {
	db := openTestDB(t)

	want := samplePreview()
	if err := db.UpsertPreview(want); err != nil {
		t.Fatalf("UpsertPreview: %v", err)
	}

	got, err := db.GetPreview(want.BeadID)
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if got == nil {
		t.Fatal("GetPreview returned no preview")
	}
	if got.Anvil != want.Anvil || got.Branch != want.Branch || got.WorktreePath != want.WorktreePath {
		t.Errorf("identity fields not round-tripped: %+v", got)
	}
	if got.Status != PreviewStarting {
		t.Errorf("status = %q, want %q", got.Status, PreviewStarting)
	}
	if len(got.Services) != 2 {
		t.Fatalf("got %d services, want 2", len(got.Services))
	}
	api, ok := got.Service("api")
	if !ok {
		t.Fatal("service api missing after round trip")
	}
	if api.Port != 42001 || api.PID != 1234 || api.LogPath != "/logs/preview-api.log" {
		t.Errorf("api service not round-tripped: %+v", api)
	}
	client, _ := got.Service("client")
	if !client.Entry {
		t.Error("client should be the entry service after round trip")
	}
	if got.CreatedAt.IsZero() || got.LastActiveAt.IsZero() {
		t.Errorf("timestamps not defaulted: created=%v lastActive=%v", got.CreatedAt, got.LastActiveAt)
	}
}

func TestUpsertPreviewUpdatesServicesAndKeepsCreatedAt(t *testing.T) {
	db := openTestDB(t)

	created := time.Now().Add(-2 * time.Hour).UTC()
	p := samplePreview()
	p.CreatedAt = created
	if err := db.UpsertPreview(p); err != nil {
		t.Fatalf("UpsertPreview: %v", err)
	}

	// A later snapshot carries the resolved health, and a bogus created_at.
	p.CreatedAt = time.Now()
	p.Status = PreviewDegraded
	p.Services[0].Health = PreviewServiceHealthy
	p.Services[1].Health = PreviewServiceFailed
	p.Services[1].Error = "not healthy within 60s"
	if err := db.UpsertPreview(p); err != nil {
		t.Fatalf("UpsertPreview (update): %v", err)
	}

	got, err := db.GetPreview(p.BeadID)
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if got.Status != PreviewDegraded {
		t.Errorf("status = %q, want %q", got.Status, PreviewDegraded)
	}
	if api, _ := got.Service("api"); api.Health != PreviewServiceHealthy {
		t.Errorf("api health = %q, want %q", api.Health, PreviewServiceHealthy)
	}
	client, _ := got.Service("client")
	if client.Health != PreviewServiceFailed || client.Error == "" {
		t.Errorf("client failure not persisted: %+v", client)
	}
	if diff := got.CreatedAt.Sub(created); diff > time.Second || diff < -time.Second {
		t.Errorf("created_at was rewritten on update: got %v, want %v", got.CreatedAt, created)
	}
}

func TestGetPreviewMissing(t *testing.T) {
	db := openTestDB(t)

	got, err := db.GetPreview("nope")
	if err != nil {
		t.Fatalf("GetPreview of unknown bead should not error: %v", err)
	}
	if got != nil {
		t.Errorf("GetPreview of unknown bead = %+v, want nil", got)
	}
}

func TestPreviewWithoutServicesRoundTrips(t *testing.T) {
	db := openTestDB(t)

	if err := db.UpsertPreview(Preview{BeadID: "Forge-empty"}); err != nil {
		t.Fatalf("UpsertPreview: %v", err)
	}
	got, err := db.GetPreview("Forge-empty")
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if len(got.Services) != 0 {
		t.Errorf("got %d services, want none", len(got.Services))
	}
	if got.Status != PreviewStarting {
		t.Errorf("status = %q, want the %q default", got.Status, PreviewStarting)
	}
}

func TestListPreviewsNewestFirst(t *testing.T) {
	db := openTestDB(t)

	older := samplePreview()
	older.BeadID = "Forge-old"
	older.CreatedAt = time.Now().Add(-time.Hour)
	newer := samplePreview()
	newer.BeadID = "Forge-new"
	newer.CreatedAt = time.Now()
	for _, p := range []Preview{older, newer} {
		if err := db.UpsertPreview(p); err != nil {
			t.Fatalf("UpsertPreview: %v", err)
		}
	}

	list, err := db.ListPreviews()
	if err != nil {
		t.Fatalf("ListPreviews: %v", err)
	}
	if len(list) != 2 {
		t.Fatalf("got %d previews, want 2", len(list))
	}
	if list[0].BeadID != "Forge-new" || list[1].BeadID != "Forge-old" {
		t.Errorf("wrong order: %s, %s", list[0].BeadID, list[1].BeadID)
	}
}

func TestSetPreviewStatusAndTouch(t *testing.T) {
	db := openTestDB(t)

	p := samplePreview()
	p.LastActiveAt = time.Now().Add(-time.Hour)
	if err := db.UpsertPreview(p); err != nil {
		t.Fatalf("UpsertPreview: %v", err)
	}

	ok, err := db.SetPreviewStatus(p.BeadID, PreviewRunning)
	if err != nil || !ok {
		t.Fatalf("SetPreviewStatus = %v, %v; want true, nil", ok, err)
	}
	got, err := db.GetPreview(p.BeadID)
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if got.Status != PreviewRunning {
		t.Errorf("status = %q, want %q", got.Status, PreviewRunning)
	}
	if time.Since(got.LastActiveAt) > time.Minute {
		t.Errorf("last_active_at not refreshed by SetPreviewStatus: %v", got.LastActiveAt)
	}

	// Touch moves last_active_at without touching status.
	if _, err := db.conn.Exec(`UPDATE previews SET last_active_at = ? WHERE bead_id = ?`,
		time.Now().Add(-time.Hour).Format(dbTimeLayout), p.BeadID); err != nil {
		t.Fatalf("backdating last_active_at: %v", err)
	}
	ok, err = db.TouchPreview(p.BeadID)
	if err != nil || !ok {
		t.Fatalf("TouchPreview = %v, %v; want true, nil", ok, err)
	}
	got, err = db.GetPreview(p.BeadID)
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if time.Since(got.LastActiveAt) > time.Minute {
		t.Errorf("last_active_at not refreshed by TouchPreview: %v", got.LastActiveAt)
	}
	if got.Status != PreviewRunning {
		t.Errorf("TouchPreview changed status to %q", got.Status)
	}

	// Unknown beads are reported, not errors.
	if ok, err := db.TouchPreview("nope"); err != nil || ok {
		t.Errorf("TouchPreview(unknown) = %v, %v; want false, nil", ok, err)
	}
	if ok, err := db.SetPreviewStatus("nope", PreviewRunning); err != nil || ok {
		t.Errorf("SetPreviewStatus(unknown) = %v, %v; want false, nil", ok, err)
	}
}

func TestDeletePreview(t *testing.T) {
	db := openTestDB(t)

	p := samplePreview()
	if err := db.UpsertPreview(p); err != nil {
		t.Fatalf("UpsertPreview: %v", err)
	}
	if err := db.DeletePreview(p.BeadID); err != nil {
		t.Fatalf("DeletePreview: %v", err)
	}
	got, err := db.GetPreview(p.BeadID)
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	if got != nil {
		t.Errorf("preview still present after delete: %+v", got)
	}
	// Deleting again is a no-op, so teardown can call it unconditionally.
	if err := db.DeletePreview(p.BeadID); err != nil {
		t.Errorf("second DeletePreview: %v", err)
	}
}
