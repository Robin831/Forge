package vardebeads

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// ReadTool is Varde's live bd read (DevInfra-rjb.6): one whitelisted bd read, run read-only in the pool's workspace,
// answered as {exitCode, stdout, stderr} with bd's stdout unchanged.
const ReadTool = "beads_bd_read"

// The shapes Varde's beads_bd_read accepts (varde/app/bd-read.mjs, readArgs). Kept in step by hand: a read this
// lets through and Varde refuses fails loudly with a 400, one this refuses never leaves the Forge.
var (
	readID       = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9._-]{0,80}$`)
	readLabels   = regexp.MustCompile(`^[A-Za-z0-9:._-]{1,100}(,[A-Za-z0-9:._-]{1,100}){0,9}$`)
	readLimit    = regexp.MustCompile(`^(0|[1-9][0-9]{0,3})$`)
	readStatuses = map[string]bool{"open": true, "in_progress": true, "blocked": true, "deferred": true, "hooked": true, "closed": true, "pinned": true}
	readValues   = map[string]map[string]func(string) bool{
		"ready":    {"--limit": readLimit.MatchString, "--label": readLabels.MatchString, "--exclude-label": readLabels.MatchString},
		"list":     {"--status": readStatus, "--limit": readLimit.MatchString, "--label": readLabels.MatchString},
		"show":     {"--id": readID.MatchString},
		"comments": {},
	}
	readBools = map[string]map[string]bool{
		"ready": {"--json": true}, "list": {"--json": true}, "show": {"--json": true, "--include-dependents": true}, "comments": {"--json": true},
	}
	readPositional = map[string]int{"ready": 0, "list": 0, "show": 200, "comments": 1}
)

const readMaxIDs = 200

func readStatus(v string) bool {
	parts := strings.Split(v, ",")
	if len(parts) > len(readStatuses) {
		return false
	}
	for _, s := range parts {
		if !readStatuses[s] {
			return false
		}
	}
	return true
}

// IsRead reports whether a bd argument list is a read Varde's beads_bd_read runs: ready, list, show (positional ids
// or --id=, optionally --include-dependents) or comments <id>, with only the flags the Forge itself sends. Varde also
// refuses an id without the pool's prefix (400), which reaches the caller as exit 1 with no JSON, not as "no such bead".
func IsRead(args []string) bool {
	if len(args) == 0 || len(args) > 2*readMaxIDs+10 {
		return false
	}
	values, ok := readValues[args[0]]
	if !ok {
		return false
	}
	bools := readBools[args[0]]
	positional, ids, closed, limited, limitZero := 0, 0, false, false, false
	for i := 1; i < len(args); i++ {
		arg := args[i]
		if len(arg) > 200 {
			return false
		}
		if !strings.HasPrefix(arg, "-") {
			if !readID.MatchString(arg) {
				return false
			}
			positional++
			continue
		}
		flag, value, inline := strings.Cut(arg, "=")
		if bools[flag] && !inline {
			continue
		}
		check, ok := values[flag]
		if !ok {
			return false
		}
		if !inline {
			if i+1 >= len(args) {
				return false
			}
			i++
			value = args[i]
		}
		if len(value) > 200 || !check(value) {
			return false
		}
		switch flag {
		case "--id":
			ids++
		case "--status":
			closed = closed || readStatus(value) && strings.Contains(","+value+",", ",closed,")
		case "--limit":
			limited, limitZero = true, value == "0"
		}
	}
	switch {
	case positional > readPositional[args[0]], ids > readMaxIDs:
		return false
	case args[0] == "comments" && positional != 1:
		return false
	case args[0] == "show" && positional == 0 && ids == 0:
		return false
	case args[0] == "list" && closed && (!limited || limitZero):
		// Varde refuses every closed bead ever in one answer.
		return false
	}
	return true
}

// ReadAnswer is beads_bd_read's structuredContent.
type ReadAnswer struct {
	ExitCode int    `json:"exitCode"`
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
}

var refusedStatus = regexp.MustCompile(`^Refused \((\d{3})\)`)

// RefusalStatus is the HTTP-style status in a Varde refusal ("Refused (503): …"), or 0 when it carries none.
func RefusalStatus(text string) int {
	m := refusedStatus.FindStringSubmatch(strings.TrimSpace(text))
	if m == nil {
		return 0
	}
	n, _ := strconv.Atoi(m[1])
	return n
}

// ReadBackoff is the wait before each retry of a busy (503) or slow (504) read; the caller's deadline stops it sooner.
var ReadBackoff = []time.Duration{1 * time.Second, 3 * time.Second, 6 * time.Second}

// RunRead runs one bd read through Varde in pool and answers as bd would: bd's stdout and stderr, and its exit code
// as the return value, so the Forge's parsers of bd's output (the not-found JSON included) work unchanged. A refusal
// or a transport failure is a line on stderr and exit code 1.
func RunRead(ctx context.Context, c Caller, pool string, args []string, stdout, stderr io.Writer, backoff []time.Duration) int {
	if !IsRead(args) {
		fmt.Fprintf(stderr, "bd-varde: bd %s is not a read Varde runs, and the Forge runs no bd of its own under Varde\n", strings.Join(args, " "))
		return 1
	}
	if pool == "" {
		fmt.Fprintln(stderr, "bd-varde: no Varde pool for this anvil; set varde_pool on it in forge.yaml")
		return 1
	}
	for attempt := 0; ; attempt++ {
		raw, err := c.Call(ctx, ReadTool, map[string]any{"pool": pool, "args": args})
		var refused *ErrRefused
		if errors.As(err, &refused) {
			status := RefusalStatus(refused.Text)
			if (status == 503 || status == 504) && attempt < len(backoff) {
				select {
				case <-time.After(backoff[attempt]):
					continue
				case <-ctx.Done():
					fmt.Fprintf(stderr, "bd-varde: Varde was busy and the read ran out of time (%s): %s\n", ctx.Err(), refused.Text)
					return 1
				}
			}
			fmt.Fprintf(stderr, "bd-varde: Varde refused bd %s in pool %s: %s\n", args[0], pool, refused.Text)
			return 1
		}
		if err != nil {
			fmt.Fprintf(stderr, "bd-varde: Varde unreachable for bd %s in pool %s: %v\n", args[0], pool, err)
			return 1
		}
		var answer *ReadAnswer
		if err := json.Unmarshal(raw, &answer); err != nil || answer == nil {
			fmt.Fprintf(stderr, "bd-varde: Varde's answer to bd %s has no {exitCode, stdout, stderr}\n", args[0])
			return 1
		}
		io.WriteString(stdout, answer.Stdout)
		io.WriteString(stderr, answer.Stderr)
		return answer.ExitCode
	}
}
