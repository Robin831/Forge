// Temporary tool to list all open PRs with fix counters.
package main

import (
	"fmt"
	"os"

	"github.com/Robin831/Forge/internal/state"
)

func main() {
	db, err := state.Open("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()
	// Query all non-terminal AND recently closed PRs so the user sees everything.
	rows, err := db.Conn().Query(`SELECT id, number, anvil, bead_id, branch, status, ci_fix_count, review_fix_count, ci_passing
	  FROM prs WHERE status NOT IN ('merged') ORDER BY created_at`)
	if err != nil {
		fmt.Fprintf(os.Stderr, "query: %v\n", err)
		os.Exit(1)
	}
	defer rows.Close()
	found := false
	for rows.Next() {
		var id, number, cifix, revfix, cipassing int
		var anvil, beadID, branch, status string
		_ = rows.Scan(&id, &number, &anvil, &beadID, &branch, &status, &cifix, &revfix, &cipassing)
		fmt.Printf("PR #%-4d  dbid=%-4d  anvil=%-14s  bead=%-12s  status=%-12s  ci_fix=%-2d  rev_fix=%-2d  ci_passing=%d\n",
			number, id, anvil, beadID, status, cifix, revfix, cipassing)
		found = true
	}
	if !found {
		fmt.Println("No non-merged PRs found.")
	}
}
