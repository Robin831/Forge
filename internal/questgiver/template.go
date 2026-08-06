package questgiver

import (
	"errors"
	"fmt"
	"strings"
	"text/template"
)

// questTemplateData is the data quest templates expand against. It is a struct
// rather than a map so a misspelled placeholder ({{.BasURL}}) fails loudly at
// expansion time instead of rendering as "<no value>" and sending the browser
// somewhere meaningless.
type questTemplateData struct {
	// BaseURL is the root the quest runs against, with any trailing slash
	// removed so `{{.BaseURL}}/login` never produces a double slash. It is the
	// quest's own url field for an ordinary run, and the preview environment's
	// entry service URL for a run against a preview.
	BaseURL string
}

// Expand returns a copy of quest with its templates resolved against baseURL.
// The original is never modified, so the same discovered quest can be expanded
// once per preview.
//
// Expansion covers each step's url and value — the two fields that carry a
// location or a typed-in string. Selectors and assertions are left alone: they
// describe the page, not the deployment. A quest with no placeholders comes
// back unchanged, so quests that hardcode absolute URLs keep working.
//
// An empty baseURL falls back to the quest's own url field, which is what an
// ordinary (non-preview) run wants.
func Expand(quest *Quest, baseURL string) (*Quest, error) {
	if quest == nil {
		return nil, errors.New("questgiver: cannot expand a nil quest")
	}

	base := strings.TrimSpace(baseURL)
	if base == "" {
		base = strings.TrimSpace(quest.URL)
	}
	base = strings.TrimRight(base, "/")

	data := questTemplateData{BaseURL: base}

	out := *quest
	out.URL = base
	out.Steps = make([]Step, len(quest.Steps))
	copy(out.Steps, quest.Steps)

	for i := range out.Steps {
		for _, field := range []*string{&out.Steps[i].URL, &out.Steps[i].Value} {
			expanded, err := expandTemplate(*field, data)
			if err != nil {
				return nil, fmt.Errorf("questgiver: quest %q step %d: %w", quest.Name, i, err)
			}
			*field = expanded
		}
	}

	return &out, nil
}

// expandTemplate renders one field. Fields without a placeholder short-circuit
// so a value that merely happens to contain braces is never parsed as a
// template.
func expandTemplate(value string, data questTemplateData) (string, error) {
	if !strings.Contains(value, "{{") {
		return value, nil
	}
	tmpl, err := template.New("quest").Option("missingkey=error").Parse(value)
	if err != nil {
		return "", fmt.Errorf("parsing template %q: %w", value, err)
	}
	var sb strings.Builder
	if err := tmpl.Execute(&sb, data); err != nil {
		return "", fmt.Errorf("expanding template %q: %w", value, err)
	}
	return sb.String(), nil
}
