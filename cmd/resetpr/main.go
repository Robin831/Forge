// Temporary tool to reset PR fix counters in state.db
// Usage: go run ./cmd/resetpr <pr_number> [pr_number ...]
package main

import (
	"fmt"
	"os"
	"strconv"

	"github.com/Robin831/Forge/internal/state"
)

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintf(os.Stderr, "Usage: resetpr <pr_number> [pr_number ...]\n")
		os.Exit(1)
	}
	db, err := state.Open("")
	if err != nil {
		fmt.Fprintf(os.Stderr, "open db: %v\n", err)
		os.Exit(1)
	}
	defer db.Close()

	for _, arg := range os.Args[1:] {
		num, err := strconv.Atoi(arg)
		if err != nil {
			fmt.Fprintf(os.Stderr, "invalid pr number %q\n", arg)
			continue
		}
		pr, err := db.PRByNumber(num)
		if err != nil {
			fmt.Fprintf(os.Stderr, "PR #%d not found: %v\n", num, err)
			continue
		}
		if err := db.UpdatePRLifecycle(pr.ID, 0, 0, pr.CIPassing); err != nil {
			fmt.Fprintf(os.Stderr, "reset lifecycle pr #%d: %v\n", num, err)
			continue
		}
		if err := db.UpdatePRStatus(pr.ID, state.PROpen); err != nil {
			fmt.Fprintf(os.Stderr, "reset status pr #%d: %v\n", num, err)
			continue
		}
		fmt.Printf("reset PR #%d (db id %d): ci_fix_count=0 review_fix_count=0 status=open\n", num, pr.ID)
	}
}
