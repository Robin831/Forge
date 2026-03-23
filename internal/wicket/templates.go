package wicket

import (
	"bytes"
	"text/template"
)

// BeadCreatedData holds the template data for the BeadCreated comment.
type BeadCreatedData struct {
	// BeadID is the newly created bead identifier (e.g. "Forge-abc1").
	BeadID string
	// Reason is the triage AI's explanation for creating the bead.
	Reason string
}

// ClarificationNeededData holds the template data for the ClarificationNeeded
// comment.
type ClarificationNeededData struct {
	// Reason is the triage AI's explanation for requesting clarification.
	Reason string
}

// FlaggedForHumanData holds the template data for the FlaggedForHuman comment.
type FlaggedForHumanData struct {
	// Reason is the triage AI's explanation for escalating to a human.
	Reason string
}

var (
	// tmplBeadCreated is posted on an issue when a bead has been successfully
	// created from it.
	tmplBeadCreated = template.Must(template.New("bead_created").Parse(
		`🔨 **Bead created: {{ .BeadID }}**

This issue has been triaged and a work item has been queued for automated implementation.

> {{ .Reason }}

I'll update this issue when the pull request is ready.`))

	// tmplClarificationNeeded is posted when the triage AI cannot act on the
	// issue without more information from the author.
	tmplClarificationNeeded = template.Must(template.New("clarification_needed").Parse(
		`👋 **Clarification needed**

Before this issue can be worked on automatically, some additional information is required.

> {{ .Reason }}

Please update the issue description and I'll re-evaluate once the details are in place.`))

	// tmplFlaggedForHuman is posted when the triage AI determines the issue
	// requires human judgment and cannot be automated.
	tmplFlaggedForHuman = template.Must(template.New("flagged_for_human").Parse(
		`🚩 **Flagged for human review**

This issue has been flagged for manual triage and will not be processed automatically.

> {{ .Reason }}`))
)

// RenderBeadCreated renders the BeadCreated comment template with the given data.
func RenderBeadCreated(data BeadCreatedData) (string, error) {
	return renderTemplate(tmplBeadCreated, data)
}

// RenderClarificationNeeded renders the ClarificationNeeded comment template.
func RenderClarificationNeeded(data ClarificationNeededData) (string, error) {
	return renderTemplate(tmplClarificationNeeded, data)
}

// RenderFlaggedForHuman renders the FlaggedForHuman comment template.
func RenderFlaggedForHuman(data FlaggedForHumanData) (string, error) {
	return renderTemplate(tmplFlaggedForHuman, data)
}

func renderTemplate(t *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
