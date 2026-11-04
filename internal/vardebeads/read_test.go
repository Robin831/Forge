package vardebeads

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// Every read shape the Forge makes (DevInfra-rjb.6 scope note, R1-R10) passes; writes, sql and help do not.
func TestIsReadMirrorsVardesWhitelist(t *testing.T) {
	ids := make([]string, 0, 201)
	for i := 0; i < 201; i++ {
		ids = append(ids, "--id=Fhi.Metadata-x"+strings.Repeat("a", i%5))
	}
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"R1 ready", []string{"ready", "--json", "--limit", "100"}, true},
		{"R1 ready under Varde with label", []string{"ready", "--json", "--limit", "100", "--exclude-label", "needs-human,schematic-wiring", "--label", "forgeSkybert"}, true},
		{"R1 ready inline flags", []string{"ready", "--json", "--limit=100", "--label=forgeSkybert"}, true},
		{"R2 list in_progress", []string{"list", "--status=in_progress", "--json"}, true},
		{"R3 show positional", []string{"show", "Fhi.Metadata-9huq2.11", "--json"}, true},
		{"R4 show multi-id", []string{"show", "--id=Heimdall-1", "--id=Heimdall-2", "--json"}, true},
		{"R5 show dependents", []string{"show", "--id=DevInfra-rjb.6", "--json", "--include-dependents"}, true},
		{"R7/R8 list open no limit", []string{"list", "--status=open", "--limit", "0", "--json"}, true},
		{"R8 wicket list", []string{"list", "--status", "open", "--json", "--limit", "50"}, true},
		{"R9 list by label", []string{"list", "--label", "schematic:Fhi.Metadata-abc", "--json"}, true},
		{"R10 comments", []string{"comments", "Fhi.Metadata-abc", "--json"}, true},
		{"R11 doctor probe", []string{"list", "--limit", "1", "--json"}, true},
		{"R12 closed with limit", []string{"list", "--status=closed", "--limit", "50", "--json"}, true},
		{"show 200 ids", append([]string{"show", "--json"}, ids[:200]...), true},

		{"empty", nil, false},
		{"sql", []string{"sql", "--json", "SELECT 1"}, false},
		{"readonly sql", []string{"sql", "--json", "--readonly", "--quiet", "SELECT 1"}, false},
		{"help", []string{"show", "--help"}, false},
		{"update", []string{"update", "B-1", "--status=in_progress"}, false},
		{"close", []string{"close", "B-1"}, false},
		{"comments add", []string{"comments", "add", "B-1", "hi"}, false},
		{"label add", []string{"label", "add", "B-1", "x"}, false},
		{"show without id", []string{"show", "--json"}, false},
		{"show 201 ids", append([]string{"show", "--json"}, ids...), false},
		{"comments without id", []string{"comments", "--json"}, false},
		{"comments two ids", []string{"comments", "B-1", "B-2"}, false},
		{"list with id", []string{"list", "B-1"}, false},
		{"list exclude-label", []string{"list", "--exclude-label", "x"}, false},
		{"ready with status", []string{"ready", "--status", "open"}, false},
		{"unknown status", []string{"list", "--status=tombstone"}, false},
		{"closed without limit", []string{"list", "--status=closed", "--json"}, false},
		{"closed with limit 0", []string{"list", "--status=open,closed", "--limit", "0"}, false},
		{"limit too big", []string{"ready", "--limit", "10000"}, false},
		{"flag-shaped id", []string{"show", "--id=-x"}, false},
		{"global db flag", []string{"list", "--db", "/tmp/x"}, false},
		{"value missing", []string{"ready", "--limit"}, false},
		{"bool with value", []string{"show", "B-1", "--json=false"}, false},
	}
	for _, c := range cases {
		if got := IsRead(c.args); got != c.want {
			t.Errorf("%s: IsRead(%q) = %v, want %v", c.name, c.args, got, c.want)
		}
	}
}

func TestReadAndWriteNeverOverlap(t *testing.T) {
	for _, args := range [][]string{{"update", "B-1"}, {"create", "--title", "x"}, {"comments", "add", "B-1", "x"}, {"label", "add", "B-1", "x"}, {"dep", "add", "B-1", "B-2"}} {
		if IsRead(args) && IsWrite(args) {
			t.Errorf("%q is both a read and a write", args)
		}
	}
}

type scriptedVarde struct {
	answers []func() (json.RawMessage, error)
	calls   []map[string]any
}

func (s *scriptedVarde) Call(_ context.Context, tool string, args map[string]any) (json.RawMessage, error) {
	if tool != ReadTool {
		return nil, &ErrRefused{Text: "Refused (400): wrong tool " + tool}
	}
	s.calls = append(s.calls, args)
	next := s.answers[0]
	if len(s.answers) > 1 {
		s.answers = s.answers[1:]
	}
	return next()
}

func answer(code int, stdout, stderr string) func() (json.RawMessage, error) {
	return func() (json.RawMessage, error) {
		raw, _ := json.Marshal(ReadAnswer{ExitCode: code, Stdout: stdout, Stderr: stderr})
		return raw, nil
	}
}

func refusal(text string) func() (json.RawMessage, error) {
	return func() (json.RawMessage, error) { return nil, &ErrRefused{Text: text} }
}

func TestRunReadPassesBdThrough(t *testing.T) {
	notFound := `{"error":"no issue found matching \"Fhi.Metadata-zz\""}` + "\n"
	cases := []struct {
		name   string
		answer func() (json.RawMessage, error)
		code   int
		stdout string
		stderr string
	}{
		{"success", answer(0, `[{"id":"Fhi.Metadata-a"}]`, "warning: tip\n"), 0, `[{"id":"Fhi.Metadata-a"}]`, "warning: tip\n"},
		{"not found keeps exit 1 and bd's JSON", answer(1, notFound, ""), 1, notFound, ""},
		{"another exit code", answer(3, "", "boom\n"), 3, "", "boom\n"},
	}
	for _, c := range cases {
		v := &scriptedVarde{answers: []func() (json.RawMessage, error){c.answer}}
		var out, errOut bytes.Buffer
		code := RunRead(context.Background(), v, "munin", []string{"show", "Fhi.Metadata-a", "--json"}, &out, &errOut, nil)
		if code != c.code || out.String() != c.stdout || errOut.String() != c.stderr {
			t.Errorf("%s: code %d stdout %q stderr %q", c.name, code, out.String(), errOut.String())
		}
		if got := v.calls[0]; got["pool"] != "munin" || strings.Join(got["args"].([]string), " ") != "show Fhi.Metadata-a --json" {
			t.Errorf("%s: sent %v", c.name, got)
		}
	}
}

func TestRunReadRetriesBusyThenAnswers(t *testing.T) {
	v := &scriptedVarde{answers: []func() (json.RawMessage, error){
		refusal("Refused (503): the read queue is full"),
		refusal("Refused (504): bd ready took longer than 30 s"),
		answer(0, "[]\n", ""),
	}}
	var out, errOut bytes.Buffer
	code := RunRead(context.Background(), v, "heimdall", []string{"ready", "--json"}, &out, &errOut, []time.Duration{time.Millisecond, time.Millisecond, time.Millisecond})
	if code != 0 || out.String() != "[]\n" || len(v.calls) != 3 {
		t.Errorf("code %d, stdout %q, stderr %q, %d calls", code, out.String(), errOut.String(), len(v.calls))
	}
}

func TestRunReadGivesUpAfterTheBackoff(t *testing.T) {
	v := &scriptedVarde{answers: []func() (json.RawMessage, error){refusal("Refused (503): busy")}}
	var errOut bytes.Buffer
	code := RunRead(context.Background(), v, "munin", []string{"ready", "--json"}, &bytes.Buffer{}, &errOut, []time.Duration{time.Millisecond})
	if code == 0 || len(v.calls) != 2 || !strings.Contains(errOut.String(), "Refused (503)") {
		t.Errorf("code %d, %d calls, stderr %q", code, len(v.calls), errOut.String())
	}
}

func TestRunReadFailsClearlyOnARefusal(t *testing.T) {
	v := &scriptedVarde{answers: []func() (json.RawMessage, error){refusal("Refused (400): Heimdall-1 is not a bead id of pool munin (Fhi.Metadata-…)")}}
	var out, errOut bytes.Buffer
	code := RunRead(context.Background(), v, "munin", []string{"show", "Heimdall-1", "--json"}, &out, &errOut, []time.Duration{time.Millisecond})
	if code != 1 || out.Len() != 0 || len(v.calls) != 1 {
		t.Errorf("a 400 is not retried and prints nothing on stdout: code %d, %d calls, stdout %q", code, len(v.calls), out.String())
	}
	if !strings.Contains(errOut.String(), "Varde refused bd show in pool munin: Refused (400)") {
		t.Errorf("stderr %q", errOut.String())
	}
}

func TestRunReadRefusesLocallyWhatVardeWouldNotRun(t *testing.T) {
	v := &scriptedVarde{}
	var errOut bytes.Buffer
	if code := RunRead(context.Background(), v, "munin", []string{"sql", "--json", "SELECT 1"}, &bytes.Buffer{}, &errOut, nil); code != 1 || len(v.calls) != 0 {
		t.Errorf("sql: code %d, %d calls", code, len(v.calls))
	}
	if !strings.Contains(errOut.String(), "not a read Varde runs") {
		t.Errorf("stderr %q", errOut.String())
	}
	errOut.Reset()
	if code := RunRead(context.Background(), v, "", []string{"ready"}, &bytes.Buffer{}, &errOut, nil); code != 1 || !strings.Contains(errOut.String(), "varde_pool") {
		t.Errorf("no pool: code %d, stderr %q", code, errOut.String())
	}
}

func TestRefusalStatus(t *testing.T) {
	for text, want := range map[string]int{"Refused (503): busy": 503, " Refused (400): x": 400, "Refused: x": 0, "varde said no": 0} {
		if got := RefusalStatus(text); got != want {
			t.Errorf("RefusalStatus(%q) = %d, want %d", text, got, want)
		}
	}
}

// Over the wire, as Varde's PR #660 answers: a busy refusal, then bd's answer in structuredContent.
func TestRunReadOverTheWire(t *testing.T) {
	calls := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		var req struct {
			Params struct {
				Name      string         `json:"name"`
				Arguments map[string]any `json:"arguments"`
			} `json:"params"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		if req.Params.Name != ReadTool || req.Params.Arguments["pool"] != "munin" {
			t.Errorf("call %+v", req.Params)
		}
		if calls == 1 {
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Refused (503): Varde's bd reads are busy; try again"}],"isError":true}}`))
			return
		}
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"bd exited 1"}],"structuredContent":{"exitCode":1,"stdout":"{\"error\":\"no issue found\"}\n","stderr":""}}}`))
	}))
	defer srv.Close()
	c := &Client{URL: srv.URL, Key: "k", HTTP: srv.Client()}
	var out, errOut bytes.Buffer
	code := RunRead(context.Background(), c, "munin", []string{"show", "--id=Fhi.Metadata-zz", "--json"}, &out, &errOut, []time.Duration{time.Millisecond})
	if code != 1 || calls != 2 || out.String() != "{\"error\":\"no issue found\"}\n" {
		t.Errorf("code %d, %d calls, stdout %q, stderr %q", code, calls, out.String(), errOut.String())
	}
}
