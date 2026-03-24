package wicket

import (
	"bytes"
	"text/template"
)

// DispatchConfirmedData holds the template data for the DispatchConfirmed comment.
type DispatchConfirmedData struct {
	// BeadID is the bead being dispatched.
	BeadID string
}

// LabelAppliedData holds the template data for the LabelApplied comment.
type LabelAppliedData struct {
	// Tag is the label/tag that was applied.
	Tag string
	// BeadID is the associated bead identifier.
	BeadID string
}

// PRCreatedData holds the template data for the PRCreated follow-up comment.
type PRCreatedData struct {
	// PRURL is the URL of the created pull request.
	PRURL string
	// BeadID is the associated bead identifier.
	BeadID string
}

// PRMergedData holds the template data for the PRMerged auto-close comment.
type PRMergedData struct {
	// PRURL is the URL of the merged pull request.
	PRURL string
	// BaseBranch is the branch the PR was merged into.
	BaseBranch string
}

// StaleWarningData holds the template data for the stale warning comment.
type StaleWarningData struct{}

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

// GenericNonTrustedUserData holds the template data for the generic response
// posted to issues from non-trusted contributors.
type GenericNonTrustedUserData struct {
	// Author is the GitHub login of the issue author.
	Author string
}

// DuplicateData holds the template data for the Duplicate comment.
type DuplicateData struct {
	// DuplicateID is the ID of the existing bead that already covers this issue.
	DuplicateID string
}

// AlreadyFixedData holds the template data for the AlreadyFixed comment.
type AlreadyFixedData struct {
	// ReferencePR is the PR URL or bead ID that resolved the issue.
	ReferencePR string
}

// OutOfScopeData holds the template data for the OutOfScope comment.
type OutOfScopeData struct {
	// Reason is the AI's explanation for why this issue is out of scope.
	Reason string
}

var (
	// tmplDispatchConfirmed is posted on an issue when a dispatch signal is detected.
	tmplDispatchConfirmed = template.Must(template.New("dispatch_confirmed").Parse(
		`🚀 **Dispatched!**

I'll update this issue when work begins on **{{ .BeadID }}** and again when the pull request is ready.`))

	// tmplLabelApplied is posted after a "label <tag>" comment is processed.
	tmplLabelApplied = template.Must(template.New("label_applied").Parse(
		`✅ Tag **{{ .Tag }}** applied to {{ .BeadID }}.

React with 🚀 or comment "dispatch" to queue this for automated implementation.`))

	// tmplPRCreated is posted when a PR is created for a Wicket-tracked issue.
	tmplPRCreated = template.Must(template.New("pr_created").Parse(
		`🔀 **Update: Pull request created**

A pull request has been opened for this issue!

- PR: {{ .PRURL }}
- Bead: {{ .BeadID }}`))

	// tmplPRMerged is posted when a PR is merged, just before auto-closing the issue.
	tmplPRMerged = template.Must(template.New("pr_merged").Parse(
		`✅ **Fixed and merged**

This issue has been resolved in {{ .PRURL }} and merged to {{ .BaseBranch }}. Closing this issue now.`))

	// tmplStaleWarning is posted on an issue that has gone stale without a reply.
	tmplStaleWarning = template.Must(template.New("stale_warning").Parse(
		`⏰ **This issue has gone stale**

It's been a while since we asked for more details. Is this still an issue you'd like addressed?

If so, please provide the information requested above and we'll get it back in the queue. Otherwise, this issue will be closed automatically in 7 days.`))

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

	// tmplGenericNonTrustedUser is posted when a non-trusted contributor opens
	// an issue. It acknowledges the issue and prompts for useful details while
	// a maintainer reviews it.
	tmplGenericNonTrustedUser = template.Must(template.New("generic_non_trusted_user").Parse(
		`👋 Thanks for opening this issue, @{{ .Author }}! A maintainer will review it shortly.

In the meantime, if you can provide any of the following it will help speed up the review:

- **Reproduction steps** (if reporting a bug)
- **Expected vs actual behavior**
- **Environment details** (OS, version, relevant configuration)

We appreciate your contribution!`))

	// tmplDuplicate is posted when the triage AI determines the issue is
	// already covered by an existing open bead.
	tmplDuplicate = template.Must(template.New("duplicate").Parse(
		`🔁 **Duplicate issue**

This issue appears to be already tracked in **{{ .DuplicateID }}**. Please follow that work item for progress updates.

If you believe this is a distinct issue, please add more details to help differentiate it.`))

	// tmplAlreadyFixed is posted when the triage AI determines the issue
	// describes a problem that has already been resolved.
	tmplAlreadyFixed = template.Must(template.New("already_fixed").Parse(
		`✅ **Already resolved**

This issue appears to have been addressed in a previous fix: **{{ .ReferencePR }}**.

If the problem persists or this is a different scenario, please reopen with additional details.`))

	// tmplOutOfScope is posted when the triage AI determines the issue is
	// outside the project's scope.
	tmplOutOfScope = template.Must(template.New("out_of_scope").Parse(
		`🚫 **Out of scope**

After reviewing this issue, it falls outside the scope of what this project handles.

> {{ .Reason }}

If you believe this assessment is incorrect, please provide additional context and a maintainer will take another look.`))
)

// RenderDispatchConfirmed renders the DispatchConfirmed comment template.
func RenderDispatchConfirmed(data DispatchConfirmedData) (string, error) {
	return renderTemplate(tmplDispatchConfirmed, data)
}

// RenderLabelApplied renders the LabelApplied comment template.
func RenderLabelApplied(data LabelAppliedData) (string, error) {
	return renderTemplate(tmplLabelApplied, data)
}

// RenderPRCreated renders the PRCreated follow-up comment template.
func RenderPRCreated(data PRCreatedData) (string, error) {
	return renderTemplate(tmplPRCreated, data)
}

// RenderPRMerged renders the PRMerged auto-close comment template.
func RenderPRMerged(data PRMergedData) (string, error) {
	return renderTemplate(tmplPRMerged, data)
}

// RenderStaleWarning renders the stale warning comment template.
func RenderStaleWarning(data StaleWarningData) (string, error) {
	return renderTemplate(tmplStaleWarning, data)
}

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

// RenderGenericNonTrustedUser renders the generic response template posted to
// issues from non-trusted contributors.
func RenderGenericNonTrustedUser(data GenericNonTrustedUserData) (string, error) {
	return renderTemplate(tmplGenericNonTrustedUser, data)
}

// RenderDuplicate renders the Duplicate comment template.
func RenderDuplicate(data DuplicateData) (string, error) {
	return renderTemplate(tmplDuplicate, data)
}

// RenderAlreadyFixed renders the AlreadyFixed comment template.
func RenderAlreadyFixed(data AlreadyFixedData) (string, error) {
	return renderTemplate(tmplAlreadyFixed, data)
}

// RenderOutOfScope renders the OutOfScope comment template.
func RenderOutOfScope(data OutOfScopeData) (string, error) {
	return renderTemplate(tmplOutOfScope, data)
}

func renderTemplate(t *template.Template, data any) (string, error) {
	var buf bytes.Buffer
	if err := t.Execute(&buf, data); err != nil {
		return "", err
	}
	return buf.String(), nil
}
