package executil

import (
	"testing"
)

func TestDecodeJSON(t *testing.T) {
	type obj struct {
		ID string `json:"id"`
	}

	tests := []struct {
		name    string
		input   string
		wantID  string
		wantErr bool
	}{
		{
			name:   "clean JSON",
			input:  `{"id":"abc"}`,
			wantID: "abc",
		},
		{
			name:   "trailing newline",
			input:  "{\"id\":\"abc\"}\n",
			wantID: "abc",
		},
		{
			name:   "trailing log lines",
			input:  "{\"id\":\"abc\"}\n2026/04/10 16:03:42.874951 orphan detection: found 975 orphaned child issue(s)\n  Fhi.Metadata-014f [closed] Warm up\n",
			wantID: "abc",
		},
		{
			name:   "leading log line then JSON",
			input:  "some warning\n{\"id\":\"def\"}\n",
			wantID: "def",
		},
		{
			name:   "leading and trailing noise",
			input:  "log line 1\n{\"id\":\"ghi\"}\nlog line 2\n",
			wantID: "ghi",
		},
		{
			name:    "no JSON at all",
			input:   "just some text\nno json here\n",
			wantErr: true,
		},
		{
			name:    "empty input",
			input:   "",
			wantErr: true,
		},
		{
			name:   "JSON array",
			input:  "[{\"id\":\"xyz\"}]\ntrailing stuff\n",
			wantID: "",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got obj
			err := DecodeJSON([]byte(tt.input), &got)
			if tt.wantErr {
				if err == nil {
					t.Fatal("expected error, got nil")
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got.ID != tt.wantID {
				t.Errorf("ID = %q, want %q", got.ID, tt.wantID)
			}
		})
	}
}

func TestDecodeJSON_Array(t *testing.T) {
	type item struct {
		Name string `json:"name"`
	}

	input := `[{"name":"first"},{"name":"second"}]` + "\n2026/04/10 orphan warning\n"
	var got []item
	if err := DecodeJSON([]byte(input), &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 {
		t.Fatalf("got %d items, want 2", len(got))
	}
	if got[0].Name != "first" {
		t.Errorf("got[0].Name = %q, want %q", got[0].Name, "first")
	}
}
