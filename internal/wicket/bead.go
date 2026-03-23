package wicket

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os/exec"
	"strings"
	"time"

	"github.com/Robin831/Forge/internal/executil"
)

// beadCreateResult is the JSON output from `bd create --json`.
type beadCreateResult struct {
	ID string `json:"id"`
}

// CreateBead runs `bd create` with the given parameters and returns the new bead ID.
// anvilPath is the working directory for the bd command (the anvil's repo path).
func CreateBead(ctx context.Context, anvilPath, title, description, issueType string, priority int) (string, error) {
	if priority < 0 || priority > 4 {
		priority = 2
	}
	if issueType == "" {
		issueType = "task"
	}

	cmdCtx, cancel := context.WithTimeout(ctx, 3*time.Minute)
	defer cancel()

	cmd := executil.HideWindow(exec.CommandContext(cmdCtx,
		"bd", "create",
		fmt.Sprintf("--title=%s", title),
		fmt.Sprintf("--description=%s", description),
		fmt.Sprintf("--type=%s", issueType),
		fmt.Sprintf("--priority=%d", priority),
		"--json",
	))
	cmd.Dir = anvilPath

	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("bd create: %w: %s", err, stderr.String())
	}

	// bd create --json may return a JSON object or just plain output.
	outStr := strings.TrimSpace(string(out))
	var result beadCreateResult
	if err := json.Unmarshal([]byte(outStr), &result); err == nil && result.ID != "" {
		return result.ID, nil
	}

	// Fallback: extract an ID-like token from the first line of output.
	firstLine := strings.SplitN(outStr, "\n", 2)[0]
	firstLine = strings.TrimSpace(firstLine)
	if firstLine != "" {
		return firstLine, nil
	}

	return "", fmt.Errorf("bd create succeeded but returned no bead ID")
}
