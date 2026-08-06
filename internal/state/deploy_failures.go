package state

import (
	"fmt"
	"strings"
	"time"
)

// Self-deploy failure reasons. The string values match the selfdeploy package's
// FailureReason constants — the daemon adapts one to the other — and are what
// the deploy_failures table stores, so they must stay in sync.
const (
	DeployReasonDrainTimeout   = "drain_timeout"
	DeployReasonSwapFailed     = "swap_failed"
	DeployReasonRestartFailed  = "restart_failed"
	DeployReasonRollbackFailed = "rollback_failed"
)

// shaDisplayLen bounds how much of a build SHA is shown in the needs-attention
// row: enough to identify the commit, short enough to leave room for the rest
// of the line.
const shaDisplayLen = 12

// DeployFailure is one self-deploy that did not end with the new binary live
// and restarting: a deferred deploy, a failed swap, a failed restart, or a
// rollback that put the previous binary back.
//
// It is persisted because these outcomes are otherwise silent. A rollback
// restores the old binary and the daemon carries on exactly as before, so a
// merged fix can sit undeployed indefinitely, discoverable only by comparing
// `forge version` against origin/main. The row survives daemon restarts (the
// rolled-back binary is still what runs after one) and is cleared by the
// deployer itself once a later deploy gets past the same failure mode.
//
// One row per (anvil, reason): a later deferral must not overwrite the record
// of an earlier rollback, since they describe different problems and the
// rollback is the more serious of the two.
type DeployFailure struct {
	// Anvil is the self-deploy anvil (Forge's own repository).
	Anvil string
	// Unit is the systemd unit the deploy targeted.
	Unit string
	// Reason is one of the DeployReason* constants.
	Reason string
	// Detail is the human-readable failure text.
	Detail string
	// AttemptedSHA is the commit the deploy tried to put live ("" when unknown).
	AttemptedSHA string
	// RestoredSHA is the build that is live again after a rollback, when known.
	RestoredSHA string
	// RolledBack reports whether the previous binary was actually put back.
	RolledBack bool
	// FailedAt is when the failure was observed; UpdatedAt is when the row was
	// last written (they differ when the same failure recurs).
	FailedAt  time.Time
	UpdatedAt time.Time
}

// deployReasonLabels renders each reason for a human. An unknown reason falls
// back to the raw value so a future reason still reads sensibly.
var deployReasonLabels = map[string]string{
	DeployReasonDrainTimeout:   "workers did not drain",
	DeployReasonSwapFailed:     "binary swap failed",
	DeployReasonRestartFailed:  "restart failed",
	DeployReasonRollbackFailed: "restart AND rollback failed",
}

func deployReasonLabel(reason string) string {
	if l, ok := deployReasonLabels[reason]; ok {
		return l
	}
	if reason == "" {
		return "unknown failure"
	}
	return strings.ReplaceAll(reason, "_", " ")
}

// Title renders the needs-attention headline: what happened, in the words an
// operator would use to decide whether to act now.
func (f DeployFailure) Title() string {
	switch {
	case f.Reason == DeployReasonDrainTimeout:
		return "Self-deploy deferred: workers did not drain"
	case f.Reason == DeployReasonRollbackFailed:
		return "Self-deploy failed: restart AND rollback failed"
	case f.RolledBack:
		return "Self-deploy rolled back: " + deployReasonLabel(f.Reason)
	default:
		return "Self-deploy failed: " + deployReasonLabel(f.Reason)
	}
}

// Summary renders the detail line: which build was attempted, which one is
// running now, why it stopped, and when. This is the line that answers "is the
// merged fix live?" without a shell.
func (f DeployFailure) Summary() string {
	var parts []string
	if f.AttemptedSHA != "" {
		parts = append(parts, "attempted "+shortSHA(f.AttemptedSHA))
	}
	if f.RolledBack {
		restored := "previous binary"
		if f.RestoredSHA != "" {
			restored = shortSHA(f.RestoredSHA)
		}
		parts = append(parts, "restored "+restored)
	}
	if f.Detail != "" {
		parts = append(parts, f.Detail)
	}
	if !f.FailedAt.IsZero() {
		parts = append(parts, "at "+f.FailedAt.Format(time.RFC3339))
	}
	if len(parts) == 0 {
		return deployReasonLabel(f.Reason)
	}
	return strings.Join(parts, "; ")
}

// shortSHA truncates a build SHA for display, leaving short or non-SHA build
// identifiers (e.g. "dev") untouched.
func shortSHA(sha string) string {
	if len(sha) > shaDisplayLen {
		return sha[:shaDisplayLen]
	}
	return sha
}

// RecordDeployFailure persists (or refreshes) one deploy failure. Re-recording
// the same anvil+reason overwrites the previous occurrence: the newest attempt
// is the one that describes the current state of the host.
func (db *DB) RecordDeployFailure(f DeployFailure) error {
	now := time.Now()
	failedAt := f.FailedAt
	if failedAt.IsZero() {
		failedAt = now
	}
	_, err := db.conn.Exec(
		`INSERT INTO deploy_failures
		    (anvil, reason, unit, detail, attempted_sha, restored_sha, rolled_back, failed_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(anvil, reason) DO UPDATE SET
		    unit = excluded.unit,
		    detail = excluded.detail,
		    attempted_sha = excluded.attempted_sha,
		    restored_sha = excluded.restored_sha,
		    rolled_back = excluded.rolled_back,
		    failed_at = excluded.failed_at,
		    updated_at = excluded.updated_at`,
		f.Anvil, f.Reason, f.Unit, f.Detail, f.AttemptedSHA, f.RestoredSHA,
		boolToInt(f.RolledBack), failedAt.Format(dbTimeLayout), now.Format(dbTimeLayout),
	)
	return err
}

// ClearDeployFailures removes the outstanding failures for an anvil, restricted
// to the given reasons when any are supplied. It returns how many rows were
// cleared so callers can log the recovery exactly once. Clearing an anvil with
// no outstanding failures is a no-op.
func (db *DB) ClearDeployFailures(anvil string, reasons ...string) (int, error) {
	query := `DELETE FROM deploy_failures WHERE anvil = ?`
	args := []any{anvil}
	if len(reasons) > 0 {
		query += ` AND reason IN (?` + strings.Repeat(", ?", len(reasons)-1) + `)`
		for _, r := range reasons {
			args = append(args, r)
		}
	}
	res, err := db.conn.Exec(query, args...)
	if err != nil {
		return 0, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, err
	}
	return int(n), nil
}

// DeployFailures returns every outstanding self-deploy failure, newest first.
func (db *DB) DeployFailures() ([]DeployFailure, error) {
	rows, err := db.conn.Query(
		`SELECT anvil, reason, unit, detail, attempted_sha, restored_sha, rolled_back, failed_at, updated_at
		   FROM deploy_failures
		  ORDER BY failed_at DESC, anvil ASC, reason ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeployFailure
	for rows.Next() {
		var (
			f                   DeployFailure
			rolledBack          int
			failedAt, updatedAt string
		)
		if err := rows.Scan(&f.Anvil, &f.Reason, &f.Unit, &f.Detail, &f.AttemptedSHA,
			&f.RestoredSHA, &rolledBack, &failedAt, &updatedAt); err != nil {
			return nil, err
		}
		f.RolledBack = rolledBack != 0
		f.FailedAt = parseTime(failedAt)
		f.UpdatedAt = parseTime(updatedAt)
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// deployAttentionTitle is the needs-attention title for a deploy failure, kept
// as a function so the formatting stays in one place.
func deployAttentionTitle(f DeployFailure) string {
	if f.Unit == "" {
		return f.Title()
	}
	return fmt.Sprintf("%s (unit %s)", f.Title(), f.Unit)
}
