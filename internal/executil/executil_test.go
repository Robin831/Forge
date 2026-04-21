package executil

import (
	"strings"
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
			// bd's mysql driver writes diagnostics like
			// "[mysql] 2026/04/20 13:51:06 packets.go:58 read tcp ...: i/o timeout"
			// to stdout when the Dolt port-forward flaps. The leading '[' used
			// to break the slow path because bytes.IndexAny returned 0.
			name:   "mysql noise prefix",
			input:  "[mysql] 2026/04/20 13:51:06 packets.go:58 read tcp 127.0.0.1:64547->127.0.0.1:3306: i/o timeout\nWarning: failed to add dependency ...\n{\"id\":\"Fhi.Metadata-gob8k\"}\n",
			wantID: "Fhi.Metadata-gob8k",
		},
		{
			name:   "timestamped noise prefix",
			input:  "[2026-04-20 13:51:06] warning foo\n{\"id\":\"abc\"}\n",
			wantID: "abc",
		},
		{
			// First '{' is inside a Go map print and isn't valid JSON on its
			// own — the scanner must advance past it to the real payload.
			name:   "first brace is decoy",
			input:  "debug: map[key:{inner}]\n{\"id\":\"real\"}\n",
			wantID: "real",
		},
		{
			// Caller asked for an object but bd returned an array. Treat this as
			// a contract error rather than extracting a nested object.
			name:    "JSON array into struct returns error",
			input:   "[{\"id\":\"xyz\"}]\ntrailing stuff\n",
			wantErr: true,
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

func TestDecodeJSON_NoValidJSONIncludesSnippet(t *testing.T) {
	type obj struct {
		ID string `json:"id"`
	}
	input := "[mysql] total garbage no json here\n"
	var got obj
	err := DecodeJSON([]byte(input), &got)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	msg := err.Error()
	if !strings.Contains(msg, "no valid JSON found") {
		t.Errorf("error %q does not mention failure mode", msg)
	}
	if !strings.Contains(msg, "[mysql]") {
		t.Errorf("error %q does not include raw output snippet", msg)
	}
}

func TestDecodeJSON_LongInputSnippetTruncated(t *testing.T) {
	type obj struct {
		ID string `json:"id"`
	}
	// 500 bytes of noise, no JSON anywhere.
	input := strings.Repeat("x", 500)
	var got obj
	err := DecodeJSON([]byte(input), &got)
	if err == nil {
		t.Fatal("expected error, got nil")
	}
	if !strings.Contains(err.Error(), "...") {
		t.Errorf("expected truncation marker in error, got: %v", err)
	}
}

func TestDecodeJSON_ArrayWithNoisePrefix(t *testing.T) {
	type item struct {
		Name string `json:"name"`
	}
	input := "[mysql] some warning\n[{\"name\":\"first\"},{\"name\":\"second\"}]\n"
	var got []item
	if err := DecodeJSON([]byte(input), &got); err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if len(got) != 2 || got[0].Name != "first" || got[1].Name != "second" {
		t.Errorf("got %+v, want [{first} {second}]", got)
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
