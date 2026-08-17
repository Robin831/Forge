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

// Forge-bci1: a service that became healthy and later died. The exit has to
// survive the services JSON column, since the row is what a restarted daemon
// and every read path see.
func TestUpsertPreviewRoundTripsAServiceExit(t *testing.T) {
	db := openTestDB(t)

	startedAt := time.Now().Add(-10 * time.Minute).UTC().Truncate(time.Second)
	exitedAt := startedAt.Add(7*time.Minute + 31*time.Second)
	code := 1

	p := samplePreview()
	p.Status = PreviewDegraded
	p.Services[0].Health = PreviewServiceHealthy
	p.Services[0].StartedAt = startedAt
	p.Services[1].Health = PreviewServiceExited
	p.Services[1].Error = "exited (exit 1, lived 7m31s)"
	p.Services[1].StartedAt = startedAt
	p.Services[1].ExitedAt = exitedAt
	p.Services[1].ExitCode = &code
	if err := db.UpsertPreview(p); err != nil {
		t.Fatalf("UpsertPreview: %v", err)
	}

	got, err := db.GetPreview(p.BeadID)
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	client, _ := got.Service("client")
	if client.Health != PreviewServiceExited {
		t.Errorf("health = %q, want %q", client.Health, PreviewServiceExited)
	}
	if client.ExitCode == nil || *client.ExitCode != 1 {
		t.Errorf("exit_code = %v, want 1", client.ExitCode)
	}
	if !client.ExitedAt.Equal(exitedAt) || !client.StartedAt.Equal(startedAt) {
		t.Errorf("timestamps not round-tripped: started=%v exited=%v", client.StartedAt, client.ExitedAt)
	}

	// The whole point: uptime stops at the death instead of growing forever.
	if got := client.Lifetime(time.Now()); got != 7*time.Minute+31*time.Second {
		t.Errorf("Lifetime = %v, want 7m31s frozen at the exit", got)
	}
	api, _ := got.Service("api")
	if live := api.Lifetime(startedAt.Add(time.Minute)); live != time.Minute {
		t.Errorf("a running service's Lifetime = %v, want it to keep counting", live)
	}
}

// Forge-4noz: a service Kiln relaunched under `restart: on-failure` carries the
// count in the same JSON column. It has to survive the round trip because it is
// the only thing separating a service that is healthy from one that is healthy
// again — and a record written before the field existed must read as zero
// rather than fail to decode.
func TestUpsertPreviewRoundTripsServiceRestarts(t *testing.T) {
	db := openTestDB(t)

	p := samplePreview()
	p.Services[0].Health = PreviewServiceHealthy
	p.Services[1].Health = PreviewServiceHealthy
	p.Services[1].Restarts = 2
	if err := db.UpsertPreview(p); err != nil {
		t.Fatalf("UpsertPreview: %v", err)
	}

	got, err := db.GetPreview(p.BeadID)
	if err != nil {
		t.Fatalf("GetPreview: %v", err)
	}
	client, _ := got.Service("client")
	if client.Restarts != 2 {
		t.Errorf("restarts = %d, want 2", client.Restarts)
	}
	api, _ := got.Service("api")
	if api.Restarts != 0 {
		t.Errorf("restarts = %d, want 0 for a service that never died", api.Restarts)
	}
}

func TestPreviewServiceLifetime(t *testing.T) {
	now := time.Now()
	tests := []struct {
		name string
		svc  PreviewService
		want time.Duration
	}{
		{name: "never started", svc: PreviewService{}},
		{
			name: "running counts to now",
			svc:  PreviewService{StartedAt: now.Add(-30 * time.Second)},
			want: 30 * time.Second,
		},
		{
			name: "exited counts to the exit",
			svc:  PreviewService{StartedAt: now.Add(-time.Hour), ExitedAt: now.Add(-30 * time.Minute)},
			want: 30 * time.Minute,
		},
		{
			// Clock skew between the record and the reader is not a negative
			// uptime; it is no information.
			name: "an exit before the start reports nothing",
			svc:  PreviewService{StartedAt: now, ExitedAt: now.Add(-time.Second)},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.svc.Lifetime(now); got != tt.want {
				t.Errorf("Lifetime = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestPreviewServiceServing(t *testing.T) {
	for health, want := range map[string]bool{
		PreviewServiceStarting: true,
		PreviewServiceHealthy:  true,
		PreviewServiceFailed:   false,
		PreviewServiceExited:   false,
		// An unset health is what a record written before this state existed
		// carries, and what several callers construct in flight. Treating it as
		// serving keeps those paths behaving exactly as they did.
		"": true,
	} {
		if got := PreviewServiceServing(health); got != want {
			t.Errorf("PreviewServiceServing(%q) = %v, want %v", health, got, want)
		}
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
