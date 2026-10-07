package bellows

import (
	"context"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/vcs"
	"github.com/Robin831/Forge/internal/vcs/github"
)

// TestMeasureCallsPerCycle drives one bellows cycle through the real gh CLI
// against a local fake GitHub (httptest, TLS, GH_HOST/GH_REPO) and counts the
// HTTP requests it receives. Opt-in: FORGE_MEASURE_GH=1 and gh on PATH. The
// scenario is 3 repos, 7 open PRs: 2 this forge's, 2 a sibling forge's, 3
// humans'. FORGE_MEASURE_MODE=secondary-limit answers every request with a
// 403 secondary limit + Retry-After instead. It never talks to github.com: every token variable gh would send
// there is cleared, and the only host gh knows is the local server.
func TestMeasureCallsPerCycle(t *testing.T) {
	if os.Getenv("FORGE_MEASURE_GH") != "1" {
		t.Skip("set FORGE_MEASURE_GH=1 to measure gh traffic against a local fake GitHub")
	}
	if _, err := exec.LookPath("gh"); err != nil {
		t.Skip("gh not on PATH")
	}

	var mu sync.Mutex
	counts := map[string]int{}
	aliasRe := regexp.MustCompile(`pr(\d+): pullRequest`)
	prNode := func(n int) map[string]any {
		return map[string]any{
			"number": n, "state": "OPEN", "mergeable": "MERGEABLE", "headRefName": fmt.Sprintf("b%d", n),
			"headRefOid": fmt.Sprintf("sha%d", n), "isDraft": false, "url": "u", "title": fmt.Sprintf("PR %d", n),
			"statusCheckRollup": map[string]any{"nodes": []any{}},
			"reviews":           map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}, "totalCount": 0},
			"reviewThreads":     map[string]any{"nodes": []any{}, "pageInfo": map[string]any{"hasNextPage": false}},
			"reviewRequests":    map[string]any{"nodes": []any{}},
		}
	}
	srv := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var req struct {
			Query     string         `json:"query"`
			Variables map[string]any `json:"variables"`
		}
		_ = json.Unmarshal(raw, &req)
		kind := "other"
		var data any = map[string]any{}
		switch {
		case strings.Contains(req.Query, "fragment prStatus"):
			kind = "batch status query"
			repo := map[string]any{}
			for _, m := range aliasRe.FindAllStringSubmatch(req.Query, -1) {
				var n int
				fmt.Sscanf(m[1], "%d", &n)
				repo["pr"+m[1]] = prNode(n)
			}
			data = map[string]any{"repository": repo}
		case strings.Contains(req.Query, "PullRequestByNumber"):
			kind = "gh pr view"
			n, _ := req.Variables["pr_number"].(float64)
			data = map[string]any{"repository": map[string]any{"pullRequest": prNode(int(n))}}
		case strings.Contains(req.Query, "reviewThreads"):
			kind = "reviewThreads query"
			data = map[string]any{"repository": map[string]any{"pullRequest": prNode(0)}}
		case strings.Contains(req.Query, "reviewRequests"):
			kind = "reviewRequests query"
			data = map[string]any{"repository": map[string]any{"pullRequest": prNode(0)}}
		}
		mu.Lock()
		counts[kind]++
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		if os.Getenv("FORGE_MEASURE_MODE") == "secondary-limit" {
			w.Header().Set("Retry-After", "60")
			w.WriteHeader(http.StatusForbidden)
			fmt.Fprint(w, `{"message":"You have exceeded a secondary rate limit. Please wait a few minutes before you try again."}`)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
	}))
	defer srv.Close()

	tmp := t.TempDir()
	certPath := filepath.Join(tmp, "cert.pem")
	f, err := os.Create(certPath)
	if err != nil {
		t.Fatal(err)
	}
	_ = pem.Encode(f, &pem.Block{Type: "CERTIFICATE", Bytes: srv.Certificate().Raw})
	f.Close()
	host := strings.TrimPrefix(srv.URL, "https://")
	t.Setenv("GH_TOKEN", "")
	t.Setenv("GITHUB_TOKEN", "")
	t.Setenv("GH_ENTERPRISE_TOKEN", "fake-local-token")
	t.Setenv("GITHUB_ENTERPRISE_TOKEN", "")
	t.Setenv("GH_HOST", host)
	t.Setenv("GH_REPO", host+"/owner/repo")
	t.Setenv("SSL_CERT_FILE", certPath)
	t.Setenv("GH_CONFIG_DIR", filepath.Join(tmp, "ghconfig"))
	t.Setenv("GH_PROMPT_DISABLED", "1")
	t.Setenv("GH_NO_UPDATE_NOTIFIER", "1")
	t.Setenv("NO_COLOR", "1")

	paths := map[string]string{}
	for _, name := range []string{"repo-a", "repo-b", "repo-c"} {
		dir := filepath.Join(tmp, name)
		for _, args := range [][]string{{"init", "-q", dir}, {"-C", dir, "remote", "add", "origin", "https://github.com/owner/" + name + ".git"}} {
			if out, err := exec.Command("git", args...).CombinedOutput(); err != nil {
				t.Fatalf("git %v: %v\n%s", args, err, out)
			}
		}
		paths[name] = dir
	}

	db, cleanup := openTempDB(t)
	defer cleanup()
	rows := []state.PR{
		{Number: 101, Anvil: "repo-a", BeadID: "Forge-own1", Branch: "forge/Forge-own1"},
		{Number: 201, Anvil: "repo-b", BeadID: "Forge-own2", Branch: "forge/Forge-own2"},
		{Number: 102, Anvil: "repo-a", BeadID: "ext-102", Branch: "forge/Sib-1"},
		{Number: 301, Anvil: "repo-c", BeadID: "ext-301", Branch: "forge/Sib-2"},
		{Number: 103, Anvil: "repo-a", BeadID: "ext-103", Branch: "alice/fix"},
		{Number: 202, Anvil: "repo-b", BeadID: "ext-202", Branch: "bob/feature"},
		{Number: 302, Anvil: "repo-c", BeadID: "ext-302", Branch: "carol/docs"},
	}
	for i := range rows {
		rows[i].Status = state.PROpen
		rows[i].CreatedAt = time.Now()
		if err := db.InsertPR(&rows[i]); err != nil {
			t.Fatal(err)
		}
	}

	provider := github.New(nil)
	m := New(db, func(string) vcs.Provider { return provider }, time.Minute, paths, nil, nil, nil, nil)
	zero := github.RetryBackoff{}
	m.retryBackoff = &zero
	m.checkAll(context.Background())

	total := 0
	for kind, n := range counts {
		t.Logf("MEASURE %-22s %d", kind, n)
		total += n
	}
	t.Logf("MEASURE total HTTP requests per cycle: %d", total)
}
