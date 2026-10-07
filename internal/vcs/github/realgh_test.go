package github

import (
	"context"
	"encoding/pem"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// TestCheckStatusBatch_RealGHSecondaryLimit runs the batch through the real gh
// CLI against a local fake GitHub answering 403 + Retry-After, proving the
// header reaches RateLimitError through `gh api --include`. Opt-in
// (FORGE_MEASURE_GH=1, gh on PATH); no request leaves the machine.
func TestCheckStatusBatch_RealGHSecondaryLimit(t *testing.T) {
	if os.Getenv("FORGE_MEASURE_GH") != "1" {
		t.Skip("set FORGE_MEASURE_GH=1 to run against the real gh CLI and a local fake GitHub")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not on PATH")
	}
	var requests atomic.Int32
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		w.Header().Set("Retry-After", "42")
		w.Header().Set("X-Ratelimit-Remaining", "4100")
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
	}))
	defer srv.Close()
	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "cert.pem")
	f, _ := os.Create(certPath)
	_ = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	f.Close()
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "fake-local-token")
	t.Setenv("GH_HOST", strings.TrimPrefix(srv.URL, "https://"))
	t.Setenv("SSL_CERT_FILE", certPath)
	t.Setenv("GH_CONFIG_DIR", filepath.Join(tmp, "ghconfig"))
	t.Setenv("GH_PROMPT_DISABLED", "1")
	t.Setenv("GH_NO_UPDATE_NOTIFIER", "1")

	repo := filepath.Join(tmp, "repo")
	for _, args := range [][]string{{"init", "-q", repo}, {"-C", repo, "remote", "add", "origin", "https://github.com/owner/repo.git"}} {
		if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	_, err := New(nil).CheckStatusBatch(context.Background(), repo, []int{1, 2, 3})
	rl, ok := AsRateLimit(err)
	if !ok || !rl.Secondary || rl.RetryAfter != 42*time.Second {
		t.Fatalf("want a secondary RateLimitError with RetryAfter 42s, got %+v (%v)", rl, err)
	}
	if n := requests.Load(); n != 1 {
		t.Fatalf("want exactly 1 request, got %d", n)
	}
}
