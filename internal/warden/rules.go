package warden

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// RulesFileName is the per-anvil file storing learned review rules.
const RulesFileName = ".forge/warden-rules.yaml"

// SourceList is a slice of source references. It YAML-marshals as a plain
// scalar when there is exactly one element and as a flow sequence when there
// are multiple, making multi-PR attribution programmatically accessible.
// It unmarshals from either format for backward compatibility.
type SourceList []string

// String returns the sources joined by ", " for display purposes.
func (sl SourceList) String() string {
	return strings.Join(sl, ", ")
}

// MarshalYAML emits a plain scalar for a single source and a YAML flow
// sequence for multiple sources.
func (sl SourceList) MarshalYAML() (any, error) {
	switch len(sl) {
	case 0:
		return "", nil
	case 1:
		return sl[0], nil
	default:
		node := &yaml.Node{Kind: yaml.SequenceNode, Style: yaml.FlowStyle}
		for _, s := range sl {
			node.Content = append(node.Content, &yaml.Node{Kind: yaml.ScalarNode, Value: s})
		}
		return node, nil
	}
}

// UnmarshalYAML reads either a scalar string or a YAML sequence into a SourceList.
func (sl *SourceList) UnmarshalYAML(value *yaml.Node) error {
	switch value.Kind {
	case yaml.ScalarNode:
		*sl = SourceList{value.Value}
	case yaml.SequenceNode:
		var list []string
		if err := value.Decode(&list); err != nil {
			return err
		}
		*sl = SourceList(list)
	default:
		return fmt.Errorf("unexpected YAML node kind for SourceList: %v", value.Kind)
	}
	return nil
}

// Rule represents a single learned review pattern.
type Rule struct {
	ID       string     `yaml:"id"       json:"id"`
	Category string     `yaml:"category" json:"category"`
	Pattern  string     `yaml:"pattern"  json:"pattern"`
	Check    string     `yaml:"check"    json:"check"`
	Source   SourceList `yaml:"source"   json:"source"`
	Added    string     `yaml:"added"    json:"added"`
	// Paths is an optional list of doublestar glob patterns. When set, the
	// path-glob filter (see FilterRules) keeps the rule only when at least one
	// changed file matches one of these patterns. When empty, the rule passes
	// through to the next filter — backward compatible with rules written
	// before this field existed.
	Paths []string `yaml:"paths,omitempty" json:"paths,omitempty"`
}

// needsQuoting returns true if a YAML scalar value needs explicit quoting
// to avoid parse errors. It quotes values containing ": " (which YAML parsers
// treat as a key-value separator) or a literal `"`. It only quotes `#` when
// it could introduce a YAML comment — i.e. at the start of the value or
// immediately preceded by whitespace. This avoids spurious quoting of
// identifiers like "copilot:PR#130" where `#` is not comment-introducing.
func needsQuoting(s string) bool {
	if strings.Contains(s, ": ") || strings.Contains(s, `"`) {
		return true
	}
	for i, ch := range s {
		if ch == '#' && (i == 0 || s[i-1] == ' ' || s[i-1] == '\t') {
			return true
		}
	}
	return false
}

// MarshalYAML implements yaml.Marshaler for Rule, applying appropriate YAML
// styles to string fields:
//   - Scalars longer than 80 characters use a folded block scalar (>-) for
//     readability and easier editing.
//   - Shorter scalars that contain YAML-special sequences (": ", '"') or a
//     comment-introducing '#' are double-quoted.
//
// It uses a type alias to get automatic field marshaling (so future fields
// on Rule are included without updating this method), then post-processes
// the resulting yaml.Node tree.
func (r Rule) MarshalYAML() (any, error) {
	type RuleAlias Rule
	raw, err := yaml.Marshal(RuleAlias(r))
	if err != nil {
		return nil, err
	}
	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, err
	}
	if doc.Kind != yaml.DocumentNode || len(doc.Content) == 0 {
		return RuleAlias(r), nil
	}
	mapping := doc.Content[0]
	for i := 1; i < len(mapping.Content); i += 2 {
		vn := mapping.Content[i]
		if vn.Kind != yaml.ScalarNode {
			continue
		}
		if len(vn.Value) > 80 {
			vn.Style = yaml.FoldedStyle
		} else if needsQuoting(vn.Value) {
			vn.Style = yaml.DoubleQuotedStyle
		}
	}
	return mapping, nil
}

// RulesFile is the top-level structure of warden-rules.yaml.
type RulesFile struct {
	Rules []Rule `yaml:"rules"`
}

// RulesPath returns the full path to the warden rules file for an anvil.
func RulesPath(anvilPath string) string {
	return filepath.Join(anvilPath, RulesFileName)
}

// LoadRules reads the warden rules file from the anvil path.
// Returns an empty RulesFile (not an error) if the file does not exist.
func LoadRules(anvilPath string) (*RulesFile, error) {
	path := RulesPath(anvilPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &RulesFile{}, nil
		}
		return nil, fmt.Errorf("reading warden rules: %w", err)
	}

	var rf RulesFile
	if err := yaml.Unmarshal(data, &rf); err != nil {
		return nil, fmt.Errorf("parsing warden rules: %w", err)
	}
	return &rf, nil
}

// SaveRules writes the rules file to the anvil path, creating the
// .forge directory if it does not exist.
func SaveRules(anvilPath string, rf *RulesFile) error {
	path := RulesPath(anvilPath)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating .forge directory: %w", err)
	}

	data, err := yaml.Marshal(rf)
	if err != nil {
		return fmt.Errorf("marshaling warden rules: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// AddRule appends a rule to the file, skipping duplicates by ID.
// Returns true if the rule was added (not a duplicate).
func (rf *RulesFile) AddRule(r Rule) bool {
	for _, existing := range rf.Rules {
		if existing.ID == r.ID {
			return false
		}
	}
	if r.Added == "" {
		r.Added = time.Now().Format("2006-01-02")
	}
	rf.Rules = append(rf.Rules, r)
	return true
}

// RemoveRule removes a rule by ID. Returns true if a rule was removed.
func (rf *RulesFile) RemoveRule(id string) bool {
	for i, r := range rf.Rules {
		if r.ID == id {
			rf.Rules = append(rf.Rules[:i], rf.Rules[i+1:]...)
			return true
		}
	}
	return false
}

// FormatChecklist returns the rules formatted as a numbered checklist
// suitable for inclusion in a review prompt.
//
// This formatter applies no filtering and is kept for the Smith self-review
// checklist (combined-mode prompt). The Warden review prompt uses
// FormatChecklistForDiff so it can filter rules against the actual diff.
func (rf *RulesFile) FormatChecklist() string {
	if len(rf.Rules) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, r := range rf.Rules {
		fmt.Fprintf(&sb, "%d. [ ] Check: %s (pattern: %s)\n", i+1, r.Check, r.Pattern)
	}
	return sb.String()
}

// FormatChecklistForDiff returns the learned rules as a numbered checklist
// after filtering them against the supplied diff and changedFiles per cfg.
// When the filtered set is empty the result is "" (so the caller can omit the
// "Learned Review Rules" section entirely).
func (rf *RulesFile) FormatChecklistForDiff(diff string, changedFiles []string, cfg ReviewFilterConfig) string {
	if len(rf.Rules) == 0 {
		return ""
	}
	filtered := FilterRules(rf.Rules, diff, changedFiles, cfg)
	if len(filtered) == 0 {
		return ""
	}
	var sb strings.Builder
	for i, r := range filtered {
		fmt.Fprintf(&sb, "%d. [ ] Check: %s (pattern: %s)\n", i+1, r.Check, r.Pattern)
	}
	return sb.String()
}
