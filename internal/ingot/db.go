package ingot

import (
	"database/sql"
	"fmt"
	"time"
)

const dbTimeLayout = "2006-01-02T15:04:05.000000000Z07:00"

func formatTime(t time.Time) string {
	return t.UTC().Format(dbTimeLayout)
}

func parseTime(s string) (time.Time, error) {
	t, err := time.Parse(dbTimeLayout, s)
	if err != nil {
		// Fallback to RFC3339
		t, err = time.Parse(time.RFC3339Nano, s)
	}
	return t, err
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// InsertIngot inserts a new ingot row. It sets CreatedAt and UpdatedAt to now
// and writes the generated ID back to ingot.ID.
func InsertIngot(db *sql.DB, ingot *Ingot) error {
	now := time.Now()
	ingot.CreatedAt = now
	ingot.UpdatedAt = now

	res, err := db.Exec(`
		INSERT INTO ingots
			(bead_id, anvil, pr_id, worker_id, status,
			 created_at, updated_at,
			 temper_passed, temper_failed_step, temper_duration_ms,
			 pr_number, pr_url, title, branch)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		ingot.BeadID,
		ingot.Anvil,
		ingot.PRID,
		ingot.WorkerID,
		string(ingot.Status),
		formatTime(ingot.CreatedAt),
		formatTime(ingot.UpdatedAt),
		boolToInt(ingot.TemperPassed),
		ingot.TemperFailedStep,
		ingot.TemperDurationMs,
		ingot.PRNumber,
		ingot.PRURL,
		ingot.Title,
		ingot.Branch,
	)
	if err != nil {
		return fmt.Errorf("inserting ingot: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("getting ingot id: %w", err)
	}
	ingot.ID = int(id)
	return nil
}

// UpdateIngotStatus updates the status and updated_at of the ingot identified
// by (beadID, anvil).
func UpdateIngotStatus(db *sql.DB, beadID, anvil string, status Status) error {
	_, err := db.Exec(`
		UPDATE ingots SET status = ?, updated_at = ? WHERE bead_id = ? AND anvil = ?`,
		string(status), formatTime(time.Now()), beadID, anvil,
	)
	if err != nil {
		return fmt.Errorf("updating ingot status: %w", err)
	}
	return nil
}

// UpdateIngotTemperResults updates temper fields and updated_at.
func UpdateIngotTemperResults(db *sql.DB, beadID, anvil string, passed bool, failedStep string, durationMs int) error {
	_, err := db.Exec(`
		UPDATE ingots
		SET temper_passed = ?, temper_failed_step = ?, temper_duration_ms = ?, updated_at = ?
		WHERE bead_id = ? AND anvil = ?`,
		boolToInt(passed), failedStep, durationMs, formatTime(time.Now()), beadID, anvil,
	)
	if err != nil {
		return fmt.Errorf("updating ingot temper results: %w", err)
	}
	return nil
}

// UpdateIngotPR updates pr_number, pr_url, pr_id, and updated_at.
func UpdateIngotPR(db *sql.DB, beadID, anvil string, prNum int, prURL string, prID int) error {
	_, err := db.Exec(`
		UPDATE ingots
		SET pr_number = ?, pr_url = ?, pr_id = ?, updated_at = ?
		WHERE bead_id = ? AND anvil = ?`,
		prNum, prURL, prID, formatTime(time.Now()), beadID, anvil,
	)
	if err != nil {
		return fmt.Errorf("updating ingot PR: %w", err)
	}
	return nil
}

// GetIngot fetches a single ingot by (beadID, anvil), eager-loading its
// TestResults. Returns nil, nil if not found.
func GetIngot(db *sql.DB, beadID, anvil string) (*Ingot, error) {
	row := db.QueryRow(`
		SELECT id, bead_id, anvil, pr_id, worker_id, status,
		       temper_passed, temper_failed_step, temper_duration_ms,
		       pr_number, pr_url, title, branch, created_at, updated_at
		FROM ingots
		WHERE bead_id = ? AND anvil = ?`,
		beadID, anvil,
	)
	ingot, err := scanIngot(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("getting ingot: %w", err)
	}
	ingot.TestResults, err = GetTestResults(db, ingot.ID)
	if err != nil {
		return nil, fmt.Errorf("loading test results for ingot %d: %w", ingot.ID, err)
	}
	return ingot, nil
}

// GetIngotsByStatus returns ingots filtered by status, limited to limit rows.
func GetIngotsByStatus(db *sql.DB, status Status, limit int) ([]Ingot, error) {
	rows, err := db.Query(`
		SELECT id, bead_id, anvil, pr_id, worker_id, status,
		       temper_passed, temper_failed_step, temper_duration_ms,
		       pr_number, pr_url, title, branch, created_at, updated_at
		FROM ingots
		WHERE status = ?
		LIMIT ?`,
		string(status), limit,
	)
	if err != nil {
		return nil, fmt.Errorf("querying ingots by status: %w", err)
	}
	defer rows.Close()

	var ingots []Ingot
	for rows.Next() {
		ingot, err := scanIngot(rows)
		if err != nil {
			return nil, fmt.Errorf("scanning ingot: %w", err)
		}
		ingots = append(ingots, *ingot)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating ingots: %w", err)
	}
	return ingots, nil
}

// InsertTestResult inserts a test result row, setting RecordedAt to now if
// it is zero. It writes the generated ID back to tr.ID.
func InsertTestResult(db *sql.DB, tr *TestResult) error {
	if tr.RecordedAt.IsZero() {
		tr.RecordedAt = time.Now()
	}
	res, err := db.Exec(`
		INSERT INTO ingot_test_results
			(ingot_id, step_index, step_name, command, exit_code,
			 duration_ms, passed, optional, output_summary, full_output_path, recorded_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tr.IngotID,
		tr.StepIndex,
		tr.StepName,
		tr.Command,
		tr.ExitCode,
		tr.DurationMs,
		boolToInt(tr.Passed),
		boolToInt(tr.Optional),
		tr.OutputSummary,
		tr.FullOutputPath,
		formatTime(tr.RecordedAt),
	)
	if err != nil {
		return fmt.Errorf("inserting test result: %w", err)
	}
	id, err := res.LastInsertId()
	if err != nil {
		return fmt.Errorf("getting test result id: %w", err)
	}
	tr.ID = int(id)
	return nil
}

// GetTestResults fetches all test results for an ingot ordered by step_index.
func GetTestResults(db *sql.DB, ingotID int) ([]TestResult, error) {
	rows, err := db.Query(`
		SELECT id, ingot_id, step_index, step_name, command, exit_code,
		       duration_ms, passed, optional, output_summary, full_output_path, recorded_at
		FROM ingot_test_results
		WHERE ingot_id = ?
		ORDER BY step_index`,
		ingotID,
	)
	if err != nil {
		return nil, fmt.Errorf("querying test results: %w", err)
	}
	defer rows.Close()

	var results []TestResult
	for rows.Next() {
		var tr TestResult
		var passed, optional int
		var recordedAtStr string
		if err := rows.Scan(
			&tr.ID, &tr.IngotID, &tr.StepIndex, &tr.StepName, &tr.Command,
			&tr.ExitCode, &tr.DurationMs, &passed, &optional,
			&tr.OutputSummary, &tr.FullOutputPath, &recordedAtStr,
		); err != nil {
			return nil, fmt.Errorf("scanning test result: %w", err)
		}
		tr.Passed = passed == 1
		tr.Optional = optional == 1
		tr.RecordedAt, err = parseTime(recordedAtStr)
		if err != nil {
			return nil, fmt.Errorf("parsing recorded_at: %w", err)
		}
		results = append(results, tr)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterating test results: %w", err)
	}
	return results, nil
}

// scanner is satisfied by both *sql.Row and *sql.Rows.
type scanner interface {
	Scan(dest ...any) error
}

func scanIngot(s scanner) (*Ingot, error) {
	var ingot Ingot
	var prID sql.NullInt64
	var prNumber sql.NullInt64
	var temperPassed sql.NullInt64
	var createdAtStr, updatedAtStr string
	var statusStr string

	if err := s.Scan(
		&ingot.ID, &ingot.BeadID, &ingot.Anvil,
		&prID, &ingot.WorkerID, &statusStr,
		&temperPassed, &ingot.TemperFailedStep, &ingot.TemperDurationMs,
		&prNumber, &ingot.PRURL, &ingot.Title, &ingot.Branch,
		&createdAtStr, &updatedAtStr,
	); err != nil {
		return nil, err
	}

	ingot.Status = Status(statusStr)

	if prID.Valid {
		v := int(prID.Int64)
		ingot.PRID = &v
	}
	if prNumber.Valid {
		v := int(prNumber.Int64)
		ingot.PRNumber = &v
	}
	if temperPassed.Valid {
		ingot.TemperPassed = temperPassed.Int64 == 1
	}

	var err error
	ingot.CreatedAt, err = parseTime(createdAtStr)
	if err != nil {
		return nil, fmt.Errorf("parsing created_at: %w", err)
	}
	ingot.UpdatedAt, err = parseTime(updatedAtStr)
	if err != nil {
		return nil, fmt.Errorf("parsing updated_at: %w", err)
	}
	return &ingot, nil
}
