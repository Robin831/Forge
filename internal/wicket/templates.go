package wicket

import (
	"bytes"
	"strings"
	"text/template"
)

// templateData holds data for rendering comment templates.
type templateData struct {
	BeadID      string
	BeadTitle   string
	Question    string
	Reasoning   string
	IssueNumber int
	Repo        string
}

var (
	tmplBeadCreated = template.Must(template.New("bead_created").Parse(`
Thank you for filing this issue! 🔨

I've analyzed it and it looks like a clear, actionable task. I've created a work item to track the implementation:

**Bead ID**: ` + "`" + `{{.BeadID}}` + "`" + `
**Title**: {{.BeadTitle}}

An AI agent will pick this up shortly. I'll update this issue when a pull request is ready.

*— Forge Wicket (automated triage)*
`))

	tmplClarificationNeeded = template.Must(template.New("clarification_needed").Parse(`
Thank you for filing this issue!

Before I can create a work item, I need a bit more information:

{{.Question}}

Once this is clarified, I'll create a task and an AI agent will get started.

*— Forge Wicket (automated triage)*
`))

	tmplFlaggedForHuman = template.Must(template.New("flagged_for_human").Parse(`
Thank you for filing this issue!

I've flagged this for human review — it requires judgment that goes beyond automated triage.

{{- if .Reasoning}}

*Reason: {{.Reasoning}}*
{{- end}}

A team member will follow up.

*— Forge Wicket (automated triage)*
`))
)

// renderBeadCreated renders the "bead created" comment for trusted users.
func renderBeadCreated(beadID, beadTitle string) string {
	return renderTemplate(tmplBeadCreated, templateData{
		BeadID:    beadID,
		BeadTitle: beadTitle,
	})
}

// renderClarificationNeeded renders the clarification request comment.
func renderClarificationNeeded(question string) string {
	return renderTemplate(tmplClarificationNeeded, templateData{
		Question: question,
	})
}

// renderFlaggedForHuman renders the "flagged for human" comment.
func renderFlaggedForHuman(reasoning string) string {
	return renderTemplate(tmplFlaggedForHuman, templateData{
		Reasoning: reasoning,
	})
}

func renderTemplate(tmpl *template.Template, data templateData) string {
	var buf bytes.Buffer
	if err := tmpl.Execute(&buf, data); err != nil {
		// Fallback to plain text if template execution fails.
		return strings.TrimSpace(tmpl.Name()) + ": template error"
	}
	return strings.TrimSpace(buf.String())
}
