package warden

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// ArchiveFileName is the per-anvil file storing archived (superseded or
// stale) review rules.
const ArchiveFileName = ".forge/warden-rules.archive.yaml"

// Archive reason constants. ArchivedRule.ArchiveReason must be one of these.
const (
	ArchiveReasonDuplicate = "duplicate"
	ArchiveReasonStale     = "stale"
)

// ArchivedRule represents a Rule that has been retired from the active
// rules file. It embeds the original Rule and adds bookkeeping fields
// describing why and when the rule was archived.
type ArchivedRule struct {
	Rule          `yaml:",inline"`
	SupersededBy  string    `yaml:"superseded_by,omitempty" json:"superseded_by,omitempty"`
	LastSeen      time.Time `yaml:"last_seen,omitempty"     json:"last_seen,omitempty"`
	ArchivedAt    time.Time `yaml:"archived_at"             json:"archived_at"`
	ArchiveReason string    `yaml:"archive_reason"          json:"archive_reason"`
}

// MarshalYAML produces a single mapping node combining Rule's fields (via
// its custom marshaller) with the archive-specific fields. The default
// inline embedding does not work here because Rule.MarshalYAML returns a
// mapping node that would otherwise replace the entire ArchivedRule
// mapping, dropping the archive bookkeeping fields.
func (ar ArchivedRule) MarshalYAML() (any, error) {
	ruleAny, err := ar.Rule.MarshalYAML()
	if err != nil {
		return nil, err
	}
	mapping, ok := ruleAny.(*yaml.Node)
	if !ok || mapping == nil || mapping.Kind != yaml.MappingNode {
		return nil, fmt.Errorf("archive: Rule.MarshalYAML did not return a mapping node")
	}

	extras := struct {
		SupersededBy  string    `yaml:"superseded_by,omitempty"`
		LastSeen      time.Time `yaml:"last_seen,omitempty"`
		ArchivedAt    time.Time `yaml:"archived_at"`
		ArchiveReason string    `yaml:"archive_reason"`
	}{
		SupersededBy:  ar.SupersededBy,
		LastSeen:      ar.LastSeen,
		ArchivedAt:    ar.ArchivedAt,
		ArchiveReason: ar.ArchiveReason,
	}

	var extraNode yaml.Node
	if err := extraNode.Encode(extras); err != nil {
		return nil, err
	}
	if extraNode.Kind == yaml.MappingNode {
		mapping.Content = append(mapping.Content, extraNode.Content...)
	}
	return mapping, nil
}

// UnmarshalYAML decodes a mapping node into both the embedded Rule and the
// archive-specific fields. Decoding the same node twice — once into Rule
// and once into the extras struct — sidesteps the inline+custom-marshaler
// interaction that would otherwise silently drop fields.
func (ar *ArchivedRule) UnmarshalYAML(value *yaml.Node) error {
	if err := value.Decode(&ar.Rule); err != nil {
		return err
	}
	var extras struct {
		SupersededBy  string    `yaml:"superseded_by"`
		LastSeen      time.Time `yaml:"last_seen"`
		ArchivedAt    time.Time `yaml:"archived_at"`
		ArchiveReason string    `yaml:"archive_reason"`
	}
	if err := value.Decode(&extras); err != nil {
		return err
	}
	ar.SupersededBy = extras.SupersededBy
	ar.LastSeen = extras.LastSeen
	ar.ArchivedAt = extras.ArchivedAt
	ar.ArchiveReason = extras.ArchiveReason
	return nil
}

// Archive is the top-level structure of warden-rules.archive.yaml.
type Archive struct {
	Rules []ArchivedRule `yaml:"rules"`
}

// ArchivePath returns the full path to the archive file for an anvil.
func ArchivePath(anvilPath string) string {
	return filepath.Join(anvilPath, ArchiveFileName)
}

// LoadArchive reads the archive file at the given path. Returns an empty
// Archive (not an error) if the file does not exist.
func LoadArchive(path string) (*Archive, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return &Archive{}, nil
		}
		return nil, fmt.Errorf("reading warden archive: %w", err)
	}

	var a Archive
	if err := yaml.Unmarshal(data, &a); err != nil {
		return nil, fmt.Errorf("parsing warden archive: %w", err)
	}
	return &a, nil
}

// Save writes the archive to the given file path, creating the parent
// directory if it does not exist.
func (a *Archive) Save(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("creating archive directory: %w", err)
	}

	data, err := yaml.Marshal(a)
	if err != nil {
		return fmt.Errorf("marshaling warden archive: %w", err)
	}
	return os.WriteFile(path, data, 0o644)
}

// Add appends a rule to the archive, recording the reason it was archived
// and (optionally) the ID of the rule that supersedes it. If an entry with
// the same ID already exists, it is replaced in place so the archive stays
// deduplicated by ID. Both ArchivedAt and LastSeen are set to time.Now() at
// archive time (Rule has no last-seen field yet; a future pass will propagate
// one when per-rule activity tracking is introduced).
func (a *Archive) Add(rule Rule, reason, supersededBy string) {
	now := time.Now().UTC()
	entry := ArchivedRule{
		Rule:          rule,
		SupersededBy:  supersededBy,
		LastSeen:      now,
		ArchivedAt:    now,
		ArchiveReason: reason,
	}

	for i, existing := range a.Rules {
		if existing.ID == rule.ID {
			a.Rules[i] = entry
			return
		}
	}
	a.Rules = append(a.Rules, entry)
}

// Find returns the archived rule with the given ID and true when present.
func (a *Archive) Find(id string) (*ArchivedRule, bool) {
	for i := range a.Rules {
		if a.Rules[i].ID == id {
			return &a.Rules[i], true
		}
	}
	return nil, false
}

// Remove deletes the archived rule with the given ID and returns it. The
// second return value reports whether a rule was found and removed.
func (a *Archive) Remove(id string) (*ArchivedRule, bool) {
	for i, r := range a.Rules {
		if r.ID == id {
			removed := r
			a.Rules = append(a.Rules[:i], a.Rules[i+1:]...)
			return &removed, true
		}
	}
	return nil, false
}
