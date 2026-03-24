package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
)

func TestPrintWicketList_Empty(t *testing.T) {
	// Capture stdout
	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWicketList(nil)

	w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	io.Copy(&buf, r)

	out := buf.String()
	if !strings.Contains(out, "No wicket issues found") {
		t.Errorf("expected empty-list message, got: %q", out)
	}
}

func TestPrintWicketList_TableFormatting(t *testing.T) {
	issues := []ipc.WicketIssueItem{
		{
			ID:           1,
			Repo:         "owner/repo",
			IssueNumber:  42,
			Title:        "Fix the thing",
			Author:       "alice",
			State:        "bead_created",
			TriageAction: "create_bead",
			BeadID:       "BD-123",
		},
		{
			ID:           2,
			Repo:         "owner/other",
			IssueNumber:  7,
			Title:        "Another issue",
			Author:       "bob",
			State:        "rejected",
			TriageAction: "reject",
			TriageReason: "out of scope",
		},
	}

	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWicketList(issues)

	w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	io.Copy(&buf, r)

	out := buf.String()

	// Check header
	if !strings.Contains(out, "REPO") {
		t.Error("expected REPO column header")
	}
	if !strings.Contains(out, "ISSUE") {
		t.Error("expected ISSUE column header")
	}
	if !strings.Contains(out, "STATE") {
		t.Error("expected STATE column header")
	}
	if !strings.Contains(out, "ACTION") {
		t.Error("expected ACTION column header")
	}
	if !strings.Contains(out, "TITLE") {
		t.Error("expected TITLE column header")
	}

	// Check data rows
	if !strings.Contains(out, "owner/repo") {
		t.Error("expected owner/repo in output")
	}
	if !strings.Contains(out, "#42") {
		t.Error("expected #42 in output")
	}
	if !strings.Contains(out, "bead_created") {
		t.Error("expected bead_created state in output")
	}
	if !strings.Contains(out, "Fix the thing") {
		t.Error("expected title in output")
	}

	// Check summary line
	if !strings.Contains(out, "2 issue(s)") {
		t.Errorf("expected summary line, got: %q", out)
	}
}

func TestPrintWicketList_TitleTruncation(t *testing.T) {
	longTitle := strings.Repeat("x", 80)
	issues := []ipc.WicketIssueItem{
		{
			Repo:        "owner/repo",
			IssueNumber: 1,
			Title:       longTitle,
			State:       "pending",
		},
	}

	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWicketList(issues)

	w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	io.Copy(&buf, r)

	out := buf.String()
	// Long titles should be truncated with ellipsis
	if !strings.Contains(out, "...") {
		t.Error("expected truncated title with ellipsis")
	}
	// Should not contain the full 80-char title
	if strings.Contains(out, longTitle) {
		t.Error("expected title to be truncated, but full title found in output")
	}
}

func TestPrintWicketList_SingleIssue(t *testing.T) {
	issues := []ipc.WicketIssueItem{
		{
			Repo:        "myorg/myrepo",
			IssueNumber: 99,
			Title:       "Single issue",
			State:       "ask_clarify",
		},
	}

	orig := os.Stdout
	r, w, _ := os.Pipe()
	os.Stdout = w

	printWicketList(issues)

	w.Close()
	os.Stdout = orig
	var buf bytes.Buffer
	io.Copy(&buf, r)

	out := buf.String()
	if !strings.Contains(out, "1 issue(s)") {
		t.Errorf("expected '1 issue(s)', got: %q", out)
	}
	if !strings.Contains(out, fmt.Sprintf("#%d", 99)) {
		t.Error("expected issue number #99")
	}
}
