package questgiver

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestParseQuest_Valid(t *testing.T) {
	q, err := ParseQuest(filepath.Join("testdata", "valid-quest.yaml"))
	if err != nil {
		t.Fatalf("ParseQuest returned error: %v", err)
	}
	if q.Name != "Login and create image" {
		t.Errorf("Name = %q, want %q", q.Name, "Login and create image")
	}
	if q.Description != "Verify a user can log in and create a new image" {
		t.Errorf("Description = %q", q.Description)
	}
	if q.URL != "http://localhost:3000" {
		t.Errorf("URL = %q", q.URL)
	}
	if len(q.Tags) != 3 {
		t.Fatalf("Tags length = %d, want 3", len(q.Tags))
	}
	wantTags := []string{"smoke", "auth", "images"}
	for i, tag := range wantTags {
		if q.Tags[i] != tag {
			t.Errorf("Tags[%d] = %q, want %q", i, q.Tags[i], tag)
		}
	}
	if len(q.Steps) != 5 {
		t.Fatalf("Steps length = %d, want 5", len(q.Steps))
	}
	if q.Steps[0].Action != "navigate" || q.Steps[0].URL != "{{.BaseURL}}/login" {
		t.Errorf("Step 0: action=%q url=%q", q.Steps[0].Action, q.Steps[0].URL)
	}
	if q.Steps[1].Action != "fill" || q.Steps[1].Selector != "#email" || q.Steps[1].Value != "test@example.com" {
		t.Errorf("Step 1: %+v", q.Steps[1])
	}
	if q.Steps[3].Timeout != 10*time.Second {
		t.Errorf("Step 3 Timeout = %v, want 10s", q.Steps[3].Timeout)
	}
	if q.Steps[4].Contains != "Image created" {
		t.Errorf("Step 4 Contains = %q", q.Steps[4].Contains)
	}
	if q.FilePath != filepath.Join("testdata", "valid-quest.yaml") {
		t.Errorf("FilePath = %q", q.FilePath)
	}
}

func TestParseQuest_InvalidYAML(t *testing.T) {
	tmp := t.TempDir()
	path := filepath.Join(tmp, "bad.yaml")
	if err := os.WriteFile(path, []byte(":\n  :\n  - [invalid: yaml: {{"), 0644); err != nil {
		t.Fatal(err)
	}
	_, err := ParseQuest(path)
	if err == nil {
		t.Fatal("expected error for invalid YAML, got nil")
	}
}

func TestParseQuest_FileNotFound(t *testing.T) {
	_, err := ParseQuest(filepath.Join(t.TempDir(), "nonexistent.yaml"))
	if err == nil {
		t.Fatal("expected error for missing file, got nil")
	}
}

func TestDiscoverQuests_MultipleFiles(t *testing.T) {
	tmp := t.TempDir()
	dir := filepath.Join(tmp, ".forge", "quests")
	if err := os.MkdirAll(dir, 0755); err != nil {
		t.Fatal(err)
	}
	quest1 := []byte("name: quest1\nsteps:\n  - action: navigate\n    url: /a\n")
	quest2 := []byte("name: quest2\nsteps:\n  - action: click\n    selector: button\n")
	if err := os.WriteFile(filepath.Join(dir, "a.yaml"), quest1, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "b.yaml"), quest2, 0644); err != nil {
		t.Fatal(err)
	}
	quests, err := DiscoverQuests(tmp)
	if err != nil {
		t.Fatalf("DiscoverQuests error: %v", err)
	}
	if len(quests) != 2 {
		t.Fatalf("got %d quests, want 2", len(quests))
	}
	names := map[string]bool{}
	for _, q := range quests {
		names[q.Name] = true
		if q.FilePath == "" {
			t.Error("FilePath is empty")
		}
	}
	if !names["quest1"] || !names["quest2"] {
		t.Errorf("unexpected quest names: %v", names)
	}
}

func TestDiscoverQuests_NonExistentDir(t *testing.T) {
	quests, err := DiscoverQuests(filepath.Join(t.TempDir(), "does-not-exist"))
	if err != nil {
		t.Fatalf("expected nil error, got %v", err)
	}
	if len(quests) != 0 {
		t.Fatalf("expected empty slice, got %d quests", len(quests))
	}
}
