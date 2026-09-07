package daemon

import (
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/Robin831/Forge/internal/poller"
	"github.com/stretchr/testify/assert"
)

// newExternalRefTestDaemon builds a minimal Daemon for ensureExternalRef:
// no DB, no bd — beadShower and githubPusher are stubbed per test.
func newExternalRefTestDaemon() *Daemon {
	return &Daemon{
		logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
}

func TestEnsureExternalRef(t *testing.T) {
	bead := poller.Bead{ID: "Fhi.Metadata-80vad"}
	issueRef := "https://github.com/FHIDev/Munin/issues/5573"

	t.Run("snapshot ref wins without any lookup", func(t *testing.T) {
		d := newExternalRefTestDaemon()
		d.beadShower = func(_, _ string) ([]byte, string, error) {
			t.Fatal("bd show must not run when the snapshot has a ref")
			return nil, "", nil
		}
		b := bead
		b.ExternalRef = issueRef
		assert.Equal(t, issueRef, d.ensureExternalRef("/anvil", b))
	})

	t.Run("fresh bd show ref is used without a push", func(t *testing.T) {
		d := newExternalRefTestDaemon()
		d.beadShower = func(_, _ string) ([]byte, string, error) {
			return []byte(`{"external_ref":"` + issueRef + `"}`), "", nil
		}
		d.githubPusher = func(_, _ string) (string, error) {
			t.Fatal("bd github push must not run when bd show finds a ref")
			return "", nil
		}
		assert.Equal(t, issueRef, d.ensureExternalRef("/anvil", bead))
	})

	// Regression (Forge-jhf1): a decomposition child that missed bd's
	// per-command auto-sync reached the PR step with no external_ref and its
	// PR carried no issue reference. The PR step must create the issue itself
	// (bd github push) before opening the PR.
	t.Run("missing ref triggers bd github push and re-fetch", func(t *testing.T) {
		d := newExternalRefTestDaemon()
		pushed := false
		d.beadShower = func(_, _ string) ([]byte, string, error) {
			if !pushed {
				return []byte(`{"external_ref":""}`), "", nil
			}
			return []byte(`{"external_ref":"` + issueRef + `"}`), "", nil
		}
		d.githubPusher = func(anvilPath, beadID string) (string, error) {
			assert.Equal(t, "/anvil", anvilPath)
			assert.Equal(t, bead.ID, beadID)
			pushed = true
			return "pushed", nil
		}
		assert.Equal(t, issueRef, d.ensureExternalRef("/anvil", bead))
		assert.True(t, pushed)
	})

	t.Run("unconfigured github sync yields empty ref quietly", func(t *testing.T) {
		d := newExternalRefTestDaemon()
		d.beadShower = func(_, _ string) ([]byte, string, error) {
			return []byte(`{"external_ref":""}`), "", nil
		}
		d.githubPusher = func(_, _ string) (string, error) {
			return "Error: github.token is not configured", nil
		}
		assert.Equal(t, "", d.ensureExternalRef("/anvil", bead))
	})

	t.Run("push failure yields empty ref", func(t *testing.T) {
		d := newExternalRefTestDaemon()
		d.beadShower = func(_, _ string) ([]byte, string, error) {
			return []byte(`{"external_ref":""}`), "", nil
		}
		d.githubPusher = func(_, _ string) (string, error) {
			return "boom", errors.New("exit status 1")
		}
		assert.Equal(t, "", d.ensureExternalRef("/anvil", bead))
	})
}
