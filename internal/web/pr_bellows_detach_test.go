package web

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// muteHandler is the fake daemon the detach/resume round-trip tests dispatch
// into. It applies the two mute verbs to state.db the way the real pr_action
// handler does — the row id decides, falling back to number+anvil when the
// caller has no id, i.e. resolvePRTargetPreferID's semantics — and refuses a PR
// it cannot resolve. That is what lets the assertions be on what the PR payload
// reports after the write rather than on the dispatched command alone.
func muteHandler(rh *recordingHandler, dbOf func() *state.DB) CommandHandler {
	return func(cmd ipc.Command) ipc.Response {
		resp := rh.handle(cmd)
		if cmd.Type != "pr_action" {
			return resp
		}
		var p ipc.PRActionPayload
		if err := json.Unmarshal(cmd.Payload, &p); err != nil {
			return ipc.Response{Type: "error", Payload: []byte(`{"message":"bad payload"}`)}
		}
		var detached bool
		switch p.Action {
		case ipc.PRActionDetachBellows:
			detached = true
		case ipc.PRActionReattachBellows:
			detached = false
		default:
			return resp
		}
		db := dbOf()
		var (
			pr  *state.PR
			err error
		)
		if p.PRID > 0 {
			pr, err = db.GetPRByID(p.PRID)
		} else {
			pr, err = db.GetPRByNumber(p.Anvil, p.PRNumber)
		}
		if err != nil || pr == nil {
			return ipc.Response{Type: "error", Payload: []byte(`{"message":"PR not found"}`)}
		}
		if err := db.UpdatePRBellowsDetached(pr.ID, detached); err != nil {
			return ipc.Response{Type: "error", Payload: []byte(`{"message":"write failed"}`)}
		}
		return resp
	}
}

// insertPRWithBead inserts an open PR under the given bead id and PR number so
// a test can address both the forge-authored and the synthetic ext-* flavour.
func insertPRWithBead(t *testing.T, srv *Server, beadID string, number int) *state.PR {
	t.Helper()
	pr := &state.PR{
		Number:    number,
		Anvil:     "anvil-a",
		BeadID:    beadID,
		Branch:    "feature/" + beadID,
		Status:    state.PROpen,
		Title:     "Test PR",
		CreatedAt: time.Now().UTC(),
	}
	if err := srv.db.InsertPR(pr); err != nil {
		t.Fatalf("insert pr: %v", err)
	}
	return pr
}

// fetchPRItem re-fetches GET /api/prs/all and returns the row with the given
// DB id from whichever section holds it. Detaching does not move a PR between
// sections — a muted PR is unwatched, not unmanaged — so the lookup spans all
// three rather than assuming one.
func fetchPRItem(t *testing.T, srv *Server, cookie string, prID int) prItemJSON {
	t.Helper()
	req := httptest.NewRequest("GET", "/api/prs/all", nil)
	req.AddCookie(&http.Cookie{Name: "forge_session", Value: cookie})
	rec := httptest.NewRecorder()
	srv.routes().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("prs/all: expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	var resp prsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("parse prs/all: %v", err)
	}
	for _, section := range [][]prItemJSON{resp.ForgePRs, resp.ExternalPRs, resp.RecentlyMerged} {
		for _, item := range section {
			if item.ID == prID {
				return item
			}
		}
	}
	t.Fatalf("PR id %d not present in any section of %s", prID, rec.Body.String())
	return prItemJSON{}
}

// TestPRBellowsDetachResume_RoundTrip is the end-to-end contract: detaching
// flips bellows_detached true in the very payload the /prs tab reads, and
// resuming flips it back. Both PR flavours are covered because they take
// different branches on the daemon side — a forge-authored row carries a real
// bead id, an ext-* row is one the reconcile loop synthesised — and the mute
// has to be durable for both.
func TestPRBellowsDetachResume_RoundTrip(t *testing.T) {
	cases := []struct {
		name   string
		beadID string
		number int
	}{
		{name: "forge-authored PR", beadID: "Forge-aaaa", number: 101},
		{name: "external PR", beadID: "ext-77", number: 77},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			rh := &recordingHandler{}
			// The fake daemon writes to the same state.db the server reads,
			// which only exists once the server does — hence the indirection.
			var live *state.DB
			srv := newServerWithDefaults(t, muteHandler(rh, func() *state.DB { return live }))
			live = srv.db
			cookie := loginAndGetCookie(t, srv)
			pr := insertPRWithBead(t, srv, tc.beadID, tc.number)

			if got := fetchPRItem(t, srv, cookie, pr.ID); got.BellowsDetached {
				t.Fatalf("a freshly inserted PR must not report bellows_detached: %+v", got)
			}

			rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/bellows/detach", pr.ID), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("detach: expected 200, got %d body=%s", rec.Code, rec.Body.String())
			}
			if got := fetchPRItem(t, srv, cookie, pr.ID); !got.BellowsDetached {
				t.Errorf("after detach: expected bellows_detached=true, got %+v", got)
			}

			rec = postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/bellows/resume", pr.ID), nil)
			if rec.Code != http.StatusOK {
				t.Fatalf("resume: expected 200, got %d body=%s", rec.Code, rec.Body.String())
			}
			if got := fetchPRItem(t, srv, cookie, pr.ID); got.BellowsDetached {
				t.Errorf("after resume: expected bellows_detached=false, got %+v", got)
			}
		})
	}
}

// TestPRBellowsDetach_DispatchesIPC pins the wire verb and the addressing the
// daemon resolves through resolvePRTargetPreferID: the row id AND the
// number+anvil pair travel together, so the daemon can fall back to the number
// for a PR the dashboard knows by number alone.
func TestPRBellowsDetach_DispatchesIPC(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "Forge-aaaa", "feature/x")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/bellows/detach", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	if cmd.Type != "pr_action" {
		t.Fatalf("expected pr_action, got %s", cmd.Type)
	}
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != ipc.PRActionDetachBellows {
		t.Errorf("expected action=%s, got %q", ipc.PRActionDetachBellows, p.Action)
	}
	if p.PRID != prID || p.PRNumber != 101 || p.Anvil != "anvil-a" {
		t.Errorf("payload must carry both addressing forms: %+v", p)
	}
}

func TestPRBellowsResume_DispatchesIPC(t *testing.T) {
	rh := &recordingHandler{}
	srv := newServerWithDefaults(t, rh.handle)
	cookie := loginAndGetCookie(t, srv)
	prID := insertOpenPR(t, srv, "ext-101", "patch")

	rec := postAction(t, srv, cookie, fmt.Sprintf("/api/prs/%d/bellows/resume", prID), nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200, got %d body=%s", rec.Code, rec.Body.String())
	}
	cmd, _ := rh.lastCommand()
	var p ipc.PRActionPayload
	_ = json.Unmarshal(cmd.Payload, &p)
	if p.Action != ipc.PRActionReattachBellows {
		t.Errorf("expected action=%s, got %q", ipc.PRActionReattachBellows, p.Action)
	}
	if p.PRID != prID || p.PRNumber != 101 || p.Anvil != "anvil-a" {
		t.Errorf("payload must carry both addressing forms: %+v", p)
	}
}

// TestPRBellowsMute_BadTargets keeps the two new routes on the same error
// envelope as the rest of the file: an unparseable id is a 400 and an id no row
// answers for is a 404, both decided before anything is dispatched.
func TestPRBellowsMute_BadTargets(t *testing.T) {
	for _, path := range []string{"bellows/detach", "bellows/resume"} {
		rh := &recordingHandler{}
		srv := newServerWithDefaults(t, rh.handle)
		cookie := loginAndGetCookie(t, srv)

		rec := postAction(t, srv, cookie, "/api/prs/abc/"+path, nil)
		if rec.Code != http.StatusBadRequest {
			t.Errorf("%s with an invalid id: expected 400, got %d body=%s", path, rec.Code, rec.Body.String())
		}
		rec = postAction(t, srv, cookie, "/api/prs/9999/"+path, nil)
		if rec.Code != http.StatusNotFound {
			t.Errorf("%s with an unknown id: expected 404, got %d body=%s", path, rec.Code, rec.Body.String())
		}
		if _, dispatched := rh.lastCommand(); dispatched {
			t.Errorf("%s: a refused target must not reach the daemon", path)
		}
	}
}
