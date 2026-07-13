package state

import (
	"path/filepath"
	"testing"
)

func openSettingsTestDB(t *testing.T) *DB {
	t.Helper()
	db, err := Open(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatalf("opening test db: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func TestSettings_GetMissingKey(t *testing.T) {
	db := openSettingsTestDB(t)

	value, ok, err := db.GetSetting("does_not_exist")
	if err != nil {
		t.Fatalf("GetSetting returned error for missing key: %v", err)
	}
	if ok {
		t.Errorf("expected ok=false for missing key, got true")
	}
	if value != "" {
		t.Errorf("expected empty value for missing key, got %q", value)
	}
}

func TestSettings_SetAndGetRoundTrip(t *testing.T) {
	db := openSettingsTestDB(t)

	if err := db.SetSetting(SettingDispatchPaused, "1"); err != nil {
		t.Fatalf("SetSetting: %v", err)
	}

	value, ok, err := db.GetSetting(SettingDispatchPaused)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true after Set")
	}
	if value != "1" {
		t.Errorf("expected value=%q, got %q", "1", value)
	}
}

func TestSettings_UpsertOverwrite(t *testing.T) {
	db := openSettingsTestDB(t)

	if err := db.SetSetting(SettingDispatchPaused, "1"); err != nil {
		t.Fatalf("first SetSetting: %v", err)
	}
	if err := db.SetSetting(SettingDispatchPaused, "0"); err != nil {
		t.Fatalf("second SetSetting: %v", err)
	}

	value, ok, err := db.GetSetting(SettingDispatchPaused)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true after upsert")
	}
	if value != "0" {
		t.Errorf("expected upsert to overwrite value to %q, got %q", "0", value)
	}
}

func TestSettings_EmptyValueRoundTrip(t *testing.T) {
	db := openSettingsTestDB(t)

	// Writing an empty string (as resume_dispatch does for the paused-at
	// timestamp) must be distinguishable from a missing key: ok=true, value="".
	if err := db.SetSetting(SettingDispatchPausedAt, ""); err != nil {
		t.Fatalf("SetSetting empty: %v", err)
	}

	value, ok, err := db.GetSetting(SettingDispatchPausedAt)
	if err != nil {
		t.Fatalf("GetSetting: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok=true for explicitly-set empty value")
	}
	if value != "" {
		t.Errorf("expected empty value, got %q", value)
	}
}
