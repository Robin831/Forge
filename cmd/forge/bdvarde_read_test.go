package main

import (
	"bytes"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// The child half of TestBdVardeReadAnswersAsBd: it runs `forge bd-varde --read` as the daemon would.
func TestBdVardeReadHelper(t *testing.T) {
	if os.Getenv("FORGE_TEST_BD_VARDE_HELPER") != "1" {
		t.Skip("helper process only")
	}
	rootCmd.SetArgs(strings.Split(os.Getenv("FORGE_TEST_BD_VARDE_ARGS"), "\x1f"))
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
	os.Exit(0)
}

// bd-varde --read prints bd's stdout and stderr as Varde passed them and exits with bd's code (DevInfra-rjb.6).
func TestBdVardeReadAnswersAsBd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body bytes.Buffer
		_, _ = body.ReadFrom(r.Body)
		switch {
		case !strings.Contains(body.String(), `"pool":"munin"`):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Refused (404): wrong pool"}],"isError":true}}`))
		case strings.Contains(body.String(), "Heimdall-1"):
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[{"type":"text","text":"Refused (400): Heimdall-1 is not a bead id of pool munin"}],"isError":true}}`))
		default:
			_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{"content":[],"structuredContent":{"exitCode":1,"stdout":"{\"error\":\"no issue found\"}\n","stderr":"bd: not found\n"}}}`))
		}
	}))
	defer srv.Close()
	dir := t.TempDir()
	anvil := filepath.Join(dir, "anvil")
	key := filepath.Join(dir, "key")
	forgeYAML := filepath.Join(dir, "forge.yaml")
	if err := os.MkdirAll(anvil, 0o755); err != nil {
		t.Fatal(err)
	}
	for path, body := range map[string]string{key: "k\n", forgeYAML: "anvils:\n  munin:\n    path: " + anvil + "\n    varde_pool: munin\n"} {
		if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	run := func(args ...string) (int, string, string) {
		cmd := exec.Command(os.Args[0], "-test.run=^TestBdVardeReadHelper$")
		cmd.Dir = anvil
		cmd.Env = append(os.Environ(), "FORGE_TEST_BD_VARDE_HELPER=1", "FORGE_VARDE_URL="+srv.URL, "FORGE_VARDE_KEY_FILE="+key,
			"FORGE_TEST_BD_VARDE_ARGS="+strings.Join(append([]string{"--config", forgeYAML, "bd-varde", "--base", dir, "--read", "--"}, args...), "\x1f"))
		var stdout, stderr bytes.Buffer
		cmd.Stdout, cmd.Stderr = &stdout, &stderr
		err := cmd.Run()
		var exit *exec.ExitError
		if err != nil && !errors.As(err, &exit) {
			t.Fatal(err)
		}
		return cmd.ProcessState.ExitCode(), stdout.String(), stderr.String()
	}
	code, stdout, stderr := run("show", "--id=Fhi.Metadata-zz", "--json")
	if code != 1 || stdout != "{\"error\":\"no issue found\"}\n" || !strings.Contains(stderr, "bd: not found") {
		t.Errorf("not found: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
	code, stdout, stderr = run("show", "Heimdall-1", "--json")
	if code != 1 || stdout != "" || !strings.Contains(stderr, "Refused (400)") {
		t.Errorf("refused: code %d, stdout %q, stderr %q", code, stdout, stderr)
	}
}
