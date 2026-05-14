package web

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
)

// bdShowMu serializes bdShowJSON swaps so that parallel tests cannot race
// on the package-level variable. Each test that calls stubBdShow holds the
// lock for its entire lifetime; only one such test can run at a time.
var bdShowMu sync.Mutex

// bdCommentsMu serializes bdCommentsJSON swaps for the same reason.
var bdCommentsMu sync.Mutex

// stubBdShow installs a temporary bdShowJSON implementation for the test.
// bdShowMu is locked until t.Cleanup runs, serializing parallel tests that
// use the global. The fn callback receives only the bead ID; the dir
// parameter is ignored because tests use fixed in-memory fixtures.
func stubBdShow(t *testing.T, fn func(beadID string) ([]byte, error)) {
	t.Helper()
	bdShowMu.Lock()
	prev := bdShowJSON
	bdShowJSON = func(_ context.Context, _ string, beadID string) ([]byte, error) {
		return fn(beadID)
	}
	t.Cleanup(func() {
		bdShowJSON = prev
		bdShowMu.Unlock()
	})
}

// stubBdComments installs a temporary bdCommentsJSON implementation for the
// test, mirroring stubBdShow.
func stubBdComments(t *testing.T, fn func(beadID string) ([]byte, error)) {
	t.Helper()
	bdCommentsMu.Lock()
	prev := bdCommentsJSON
	bdCommentsJSON = func(_ context.Context, _ string, beadID string) ([]byte, error) {
		return fn(beadID)
	}
	t.Cleanup(func() {
		bdCommentsJSON = prev
		bdCommentsMu.Unlock()
	})
}

// bdShowFixture builds a canned `bd show` JSON envelope for one bead and
// optionally its outgoing/incoming dep entries. It is the test equivalent
// of running `bd show <id> --json` against a Dolt database.
func bdShowFixture(id, title, status string, priority int, dependents, dependencies []map[string]any) []byte {
	entry := map[string]any{
		"id":           id,
		"title":        title,
		"status":       status,
		"priority":     priority,
		"dependents":   dependents,
		"dependencies": dependencies,
	}
	raw, _ := json.Marshal([]any{entry})
	return raw
}

func depEntry(id, title, status string, priority int) map[string]any {
	return map[string]any{
		"id":              id,
		"title":           title,
		"status":          status,
		"priority":        priority,
		"dependency_type": "blocks",
	}
}

func TestFetchBeadDeps_PopulatesBothDirections(t *testing.T) {
	stubBdShow(t, func(beadID string) ([]byte, error) {
		if beadID != "Forge-root" {
			t.Errorf("unexpected bead id: %s", beadID)
		}
		return bdShowFixture("Forge-root", "Root", "in_progress", 2,
			[]map[string]any{depEntry("Forge-child", "Child", "open", 3)},
			[]map[string]any{depEntry("Forge-parent", "Parent", "closed", 1)},
		), nil
	})

	blocks, blockedBy := fetchBeadDeps(context.Background(), "", "Forge-root", nil)
	if len(blocks) != 1 || blocks[0].BeadID != "Forge-child" {
		t.Fatalf("expected one blocks entry pointing at Forge-child, got %+v", blocks)
	}
	if blocks[0].Title != "Child" || blocks[0].Status != "open" || blocks[0].Priority != 3 {
		t.Errorf("blocks ref fields wrong: %+v", blocks[0])
	}
	if len(blockedBy) != 1 || blockedBy[0].BeadID != "Forge-parent" {
		t.Fatalf("expected one blocked_by entry pointing at Forge-parent, got %+v", blockedBy)
	}
	if blockedBy[0].Status != "closed" || blockedBy[0].Priority != 1 {
		t.Errorf("blocked_by ref fields wrong: %+v", blockedBy[0])
	}
}

func TestFetchBeadDeps_IsolatedBead(t *testing.T) {
	stubBdShow(t, func(_ string) ([]byte, error) {
		return bdShowFixture("Forge-lonely", "Lonely", "open", 3, nil, nil), nil
	})

	blocks, blockedBy := fetchBeadDeps(context.Background(), "", "Forge-lonely", nil)
	if blocks == nil || blockedBy == nil {
		t.Fatalf("expected non-nil slices, got blocks=%v blockedBy=%v", blocks, blockedBy)
	}
	if len(blocks) != 0 || len(blockedBy) != 0 {
		t.Errorf("expected empty deps, got blocks=%+v blockedBy=%+v", blocks, blockedBy)
	}
}

func TestFetchBeadDeps_BdErrorReturnsEmpty(t *testing.T) {
	stubBdShow(t, func(_ string) ([]byte, error) {
		return nil, errors.New("bd missing")
	})

	blocks, blockedBy := fetchBeadDeps(context.Background(), "", "Forge-err", nil)
	if blocks == nil || blockedBy == nil {
		t.Fatalf("expected non-nil slices on error, got nil")
	}
	if len(blocks) != 0 || len(blockedBy) != 0 {
		t.Errorf("expected empty deps on bd error, got blocks=%+v blockedBy=%+v", blocks, blockedBy)
	}
}

func TestBeadDetail_IncludesDeps(t *testing.T) {
	stubBdShow(t, func(beadID string) ([]byte, error) {
		if beadID != "Forge-graph" {
			return bdShowFixture(beadID, beadID, "open", 3, nil, nil), nil
		}
		return bdShowFixture("Forge-graph", "Graph root", "in_progress", 2,
			[]map[string]any{depEntry("Forge-down", "Downstream", "open", 3)},
			[]map[string]any{depEntry("Forge-up", "Upstream", "closed", 1)},
		), nil
	})
	stubBdComments(t, func(_ string) ([]byte, error) {
		return []byte("[]"), nil
	})

	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/Forge-graph", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var got struct {
		Blocks    []beadDetailDepRef `json:"blocks"`
		BlockedBy []beadDetailDepRef `json:"blocked_by"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Blocks) != 1 || got.Blocks[0].BeadID != "Forge-down" {
		t.Errorf("blocks: %+v", got.Blocks)
	}
	if len(got.BlockedBy) != 1 || got.BlockedBy[0].BeadID != "Forge-up" {
		t.Errorf("blocked_by: %+v", got.BlockedBy)
	}
}

func TestBeadDetail_IsolatedBeadEmptyDeps(t *testing.T) {
	stubBdShow(t, func(beadID string) ([]byte, error) {
		return bdShowFixture(beadID, "Solo", "open", 3, nil, nil), nil
	})
	stubBdComments(t, func(_ string) ([]byte, error) {
		return []byte("[]"), nil
	})

	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/Forge-solo", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Parse as raw map so we can distinguish null from [].
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse: %v", err)
	}
	for _, field := range []string{"blocks", "blocked_by"} {
		v, ok := raw[field]
		if !ok {
			t.Errorf("field %q missing", field)
			continue
		}
		if string(v) == "null" {
			t.Errorf("field %q is null; want []", field)
		}
		if !strings.HasPrefix(string(v), "[") {
			t.Errorf("field %q is not an array: %s", field, string(v))
		}
	}
}

func TestDepsEndpoint_BasicTree(t *testing.T) {
	stubBdShow(t, func(beadID string) ([]byte, error) {
		switch beadID {
		case "Forge-root":
			return bdShowFixture("Forge-root", "Root", "in_progress", 2,
				[]map[string]any{depEntry("Forge-leaf-b", "Leaf B", "open", 3)},
				[]map[string]any{depEntry("Forge-leaf-a", "Leaf A", "closed", 1)},
			), nil
		case "Forge-leaf-a", "Forge-leaf-b":
			return bdShowFixture(beadID, beadID, "open", 3, nil, nil), nil
		}
		return nil, errors.New("unexpected bead: " + beadID)
	})

	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/Forge-root/deps?depth=2", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got beadDepsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.BeadID != "Forge-root" {
		t.Errorf("bead_id: got %q", got.BeadID)
	}
	if got.Depth != 2 {
		t.Errorf("depth: got %d, want 2", got.Depth)
	}
	if len(got.Blocks) != 1 || got.Blocks[0].BeadID != "Forge-leaf-b" {
		t.Fatalf("blocks: %+v", got.Blocks)
	}
	if len(got.BlockedBy) != 1 || got.BlockedBy[0].BeadID != "Forge-leaf-a" {
		t.Fatalf("blocked_by: %+v", got.BlockedBy)
	}
	// Depth 2 walks one level past the immediate children, so the leaf's
	// own deps must have been evaluated (and found empty). The nested
	// fields use omitempty, so an empty result serialises as omitted —
	// either way len() must be 0.
	if len(got.Blocks[0].Blocks) != 0 || len(got.Blocks[0].BlockedBy) != 0 {
		t.Errorf("expected empty nested lists on leaf node, got %+v", got.Blocks[0])
	}
}

func TestDepsEndpoint_RespectsDepthCap(t *testing.T) {
	// Build a 5-deep linear chain so we can confirm depth=5 is clamped to
	// maxDepDepth (3) and stops walking past that.
	chain := []string{"Forge-1", "Forge-2", "Forge-3", "Forge-4", "Forge-5", "Forge-6"}
	var calls int32
	stubBdShow(t, func(beadID string) ([]byte, error) {
		atomic.AddInt32(&calls, 1)
		for i, id := range chain {
			if id == beadID {
				if i+1 >= len(chain) {
					return bdShowFixture(id, id, "open", 3, nil, nil), nil
				}
				next := chain[i+1]
				return bdShowFixture(id, id, "open", 3,
					[]map[string]any{depEntry(next, next, "open", 3)},
					nil,
				), nil
			}
		}
		return bdShowFixture(beadID, beadID, "open", 3, nil, nil), nil
	})

	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/Forge-1/deps?depth=5", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var got beadDepsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Depth != maxDepDepth {
		t.Errorf("expected depth clamped to %d, got %d", maxDepDepth, got.Depth)
	}
	// Walking 3 levels from Forge-1 should reach Forge-2 → Forge-3 →
	// Forge-4, but stop short of Forge-5.
	level1 := got.Blocks
	if len(level1) != 1 || level1[0].BeadID != "Forge-2" {
		t.Fatalf("level 1: %+v", level1)
	}
	level2 := level1[0].Blocks
	if len(level2) != 1 || level2[0].BeadID != "Forge-3" {
		t.Fatalf("level 2: %+v", level2)
	}
	level3 := level2[0].Blocks
	if len(level3) != 1 || level3[0].BeadID != "Forge-4" {
		t.Fatalf("level 3: %+v", level3)
	}
	// Beyond depth 3 the recursion must stop — the level-3 child should
	// have no further nested lists.
	if len(level3[0].Blocks) != 0 || len(level3[0].BlockedBy) != 0 {
		t.Errorf("depth cap violated: nested deps still populated at depth 4: %+v", level3[0])
	}
}

func TestDepsEndpoint_HandlesCycles(t *testing.T) {
	// Forge-a → Forge-b → Forge-a is the classic 2-node cycle. Without
	// the visited dedup the walker would recurse indefinitely.
	calls := map[string]int{}
	stubBdShow(t, func(beadID string) ([]byte, error) {
		calls[beadID]++
		switch beadID {
		case "Forge-a":
			return bdShowFixture("Forge-a", "A", "open", 3,
				[]map[string]any{depEntry("Forge-b", "B", "open", 3)},
				nil,
			), nil
		case "Forge-b":
			return bdShowFixture("Forge-b", "B", "open", 3,
				[]map[string]any{depEntry("Forge-a", "A", "open", 3)},
				nil,
			), nil
		}
		return nil, errors.New("unexpected: " + beadID)
	})

	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/Forge-a/deps?depth=3", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	// The root bead is seeded in `visited`, so even though Forge-b points
	// back at Forge-a the walker stops there. Forge-a itself should only
	// be fetched once (for the root) and Forge-b only once (as a child).
	if calls["Forge-a"] != 1 {
		t.Errorf("Forge-a fetched %d times; want 1", calls["Forge-a"])
	}
	if calls["Forge-b"] != 1 {
		t.Errorf("Forge-b fetched %d times; want 1", calls["Forge-b"])
	}

	var got beadDepsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if len(got.Blocks) != 1 || got.Blocks[0].BeadID != "Forge-b" {
		t.Fatalf("expected blocks=[Forge-b], got %+v", got.Blocks)
	}
	// Forge-b lists Forge-a as a dep, but the root was pre-marked as
	// visited so its children must be elided.
	subBlocks := got.Blocks[0].Blocks
	if len(subBlocks) == 1 && subBlocks[0].BeadID == "Forge-a" {
		// Including the ref is OK; what we must NOT do is recurse into
		// it again, which would have happened if calls["Forge-a"] > 1.
		if len(subBlocks[0].Blocks) != 0 {
			t.Errorf("cycle expanded: %+v", subBlocks[0])
		}
	}
}

func TestDepsEndpoint_InvalidID(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/..%2Fetc%2Fpasswd/deps", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("expected 400 for invalid id, got %d", rec.Code)
	}
}

func TestDepsEndpoint_InvalidDepth(t *testing.T) {
	stubBdShow(t, func(beadID string) ([]byte, error) {
		return bdShowFixture(beadID, beadID, "open", 3, nil, nil), nil
	})
	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/Forge-x/deps?depth=notanumber", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Errorf("non-integer depth: expected 400, got %d", rec.Code)
	}

	req = httptest.NewRequest("GET", "/api/bead/Forge-x/deps?depth=0", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec = httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("depth=0 should clamp to 1, got %d", rec.Code)
	}
	var got beadDepsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Depth != 1 {
		t.Errorf("depth=0 should clamp to 1, got %d", got.Depth)
	}
}

func TestFetchBeadDeps_FiltersNonBlockingEdges(t *testing.T) {
	// bd records several edge types (blocks, discovered-from, related, ...).
	// Only "blocks" relations belong on the Blocks/BlockedBy lists.
	stubBdShow(t, func(_ string) ([]byte, error) {
		entry := map[string]any{
			"id":       "Forge-mixed",
			"title":    "Mixed",
			"status":   "in_progress",
			"priority": 2,
			"dependents": []map[string]any{
				{
					"id":              "Forge-blocked-by-me",
					"title":           "Real downstream",
					"status":          "open",
					"priority":        3,
					"dependency_type": "blocks",
				},
				{
					"id":              "Forge-spawned",
					"title":           "Discovered-from sibling",
					"status":          "open",
					"priority":        3,
					"dependency_type": "discovered-from",
				},
			},
			"dependencies": []map[string]any{
				{
					"id":              "Forge-blocks-me",
					"title":           "Real upstream",
					"status":          "closed",
					"priority":        1,
					"dependency_type": "blocks",
				},
				{
					"id":              "Forge-loose-link",
					"title":           "Related, not blocking",
					"status":          "open",
					"priority":        3,
					"dependency_type": "related",
				},
			},
		}
		raw, _ := json.Marshal([]any{entry})
		return raw, nil
	})

	blocks, blockedBy := fetchBeadDeps(context.Background(), "", "Forge-mixed", nil)
	if len(blocks) != 1 || blocks[0].BeadID != "Forge-blocked-by-me" {
		t.Errorf("blocks should contain only the blocking downstream, got %+v", blocks)
	}
	if len(blockedBy) != 1 || blockedBy[0].BeadID != "Forge-blocks-me" {
		t.Errorf("blocked_by should contain only the blocking upstream, got %+v", blockedBy)
	}
}

func TestDepsEndpoint_RequiresAuth(t *testing.T) {
	srv := newServerWithDefaults(t, nil)
	req := httptest.NewRequest("GET", "/api/bead/Forge-x/deps", nil)
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Errorf("expected 401, got %d", rec.Code)
	}
}

// bdShowFixtureWithNotes mirrors bdShowFixture but injects a `notes` field
// alongside the rest of the show payload, matching how bd renders beads with
// `--append-notes` content.
func bdShowFixtureWithNotes(id, title, status string, priority int, notes string, dependents, dependencies []map[string]any) []byte {
	entry := map[string]any{
		"id":           id,
		"title":        title,
		"status":       status,
		"priority":     priority,
		"notes":        notes,
		"dependents":   dependents,
		"dependencies": dependencies,
	}
	raw, _ := json.Marshal([]any{entry})
	return raw
}

func TestBeadDetail_PopulatesNotesAndComments(t *testing.T) {
	const notes = "First note line\nSecond note line"
	stubBdShow(t, func(beadID string) ([]byte, error) {
		if beadID != "Forge-notes" {
			t.Errorf("unexpected bead id for show: %s", beadID)
		}
		return bdShowFixtureWithNotes("Forge-notes", "Notes bead", "in_progress", 2, notes, nil, nil), nil
	})
	stubBdComments(t, func(beadID string) ([]byte, error) {
		if beadID != "Forge-notes" {
			t.Errorf("unexpected bead id for comments: %s", beadID)
		}
		payload := `[
			{"id":"c1","issue_id":"Forge-notes","author":"Alice","text":"first comment","created_at":"2026-05-13T10:00:00Z"},
			{"id":"c2","issue_id":"Forge-notes","author":"Bob","text":"second comment","created_at":"2026-05-13T11:00:00Z"}
		]`
		return []byte(payload), nil
	})

	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/Forge-notes", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}

	var got beadDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Notes != notes {
		t.Errorf("notes mismatch: got %q want %q", got.Notes, notes)
	}
	if len(got.Comments) != 2 {
		t.Fatalf("expected 2 comments, got %d (%+v)", len(got.Comments), got.Comments)
	}
	want := []beadDetailComment{
		{ID: "c1", Author: "Alice", Body: "first comment", CreatedAt: "2026-05-13T10:00:00Z"},
		{ID: "c2", Author: "Bob", Body: "second comment", CreatedAt: "2026-05-13T11:00:00Z"},
	}
	for i, w := range want {
		if got.Comments[i] != w {
			t.Errorf("comment %d: got %+v want %+v", i, got.Comments[i], w)
		}
	}
}

func TestBeadDetail_CommentsFailureReturnsEmpty(t *testing.T) {
	const notes = "Notes still present"
	stubBdShow(t, func(_ string) ([]byte, error) {
		return bdShowFixtureWithNotes("Forge-cerr", "Comment err", "open", 3, notes, nil, nil), nil
	})
	stubBdComments(t, func(_ string) ([]byte, error) {
		return nil, errors.New("bd comments: exit 1")
	})

	srv := newServerWithDefaults(t, nil)
	cookie := loginAndGetCookie(t, srv)

	req := httptest.NewRequest("GET", "/api/bead/Forge-cerr", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 even when bd comments fails, got %d body=%s", rec.Code, rec.Body.String())
	}

	// Comments must be a non-null, empty array — the SPA assumes the
	// field is always iterable. Notes from the (successful) bd show call
	// must still be surfaced.
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
		t.Fatalf("parse raw: %v", err)
	}
	commentsRaw, ok := raw["comments"]
	if !ok {
		t.Fatalf("comments field missing from response: %s", rec.Body.String())
	}
	if string(commentsRaw) != "[]" {
		t.Errorf("comments should serialise as [] on failure, got %s", string(commentsRaw))
	}

	var got beadDetailResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatalf("parse: %v", err)
	}
	if got.Notes != notes {
		t.Errorf("notes lost on comments failure: got %q want %q", got.Notes, notes)
	}
	if len(got.Comments) != 0 {
		t.Errorf("expected zero comments on failure, got %+v", got.Comments)
	}
}
