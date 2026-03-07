package warden

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// RulesFileName is the per-anvil file storing learned review rules.
const RulesFileName = ".forge/warden-rules.yaml"

// Rule represents a single learned review pattern.
type Rule struct {
	ID       string `yaml:"id"       json:"id"`
	Category string `yaml:"category" json:"category"`
	Pattern  string `yaml:"pattern"  json:"pattern"`
	Check    string `yaml:"check"    json:"check"`
	Source   string `yaml:"source"   json:"source"`
	Added    string `yaml:"added"    json:"added"`
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
func (rf *RulesFile) FormatChecklist() string {
	if len(rf.Rules) == 0 {
		return ""
	}
	var out string
	for i, r := range rf.Rules {
		out += fmt.Sprintf("%d. [ ] Check: %s (pattern: %s)\n", i+1, r.Check, r.Pattern)
	}
	return out
}
