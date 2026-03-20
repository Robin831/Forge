package questgiver

import (
	"fmt"
	"os"
	"path/filepath"
	"time"

	"gopkg.in/yaml.v3"
)

// Step represents a single action in a quest sequence.
type Step struct {
	Action   string        `yaml:"action"`
	URL      string        `yaml:"url,omitempty"`
	Selector string        `yaml:"selector,omitempty"`
	Value    string        `yaml:"value,omitempty"`
	Contains string        `yaml:"contains,omitempty"`
	Timeout  time.Duration `yaml:"timeout,omitempty"`
}

// Quest represents a named sequence of browser automation steps.
type Quest struct {
	Name        string   `yaml:"name"`
	Description string   `yaml:"description"`
	URL         string   `yaml:"url"`
	Tags        []string `yaml:"tags"`
	Steps       []Step   `yaml:"steps"`
	FilePath    string   `yaml:"-"`
}

// ParseQuest reads a YAML file at path and returns the parsed Quest.
func ParseQuest(path string) (*Quest, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("questgiver: reading quest file %s: %w", path, err)
	}
	var q Quest
	if err := yaml.Unmarshal(data, &q); err != nil {
		return nil, fmt.Errorf("questgiver: parsing quest file %s: %w", path, err)
	}
	q.FilePath = path
	return &q, nil
}

// DiscoverQuests finds all quest YAML files under <anvilPath>/.forge/quests/
// and returns the parsed quests. Returns an empty slice if the directory does not exist.
func DiscoverQuests(anvilPath string) ([]Quest, error) {
	questsDir := filepath.Join(anvilPath, ".forge", "quests")

	info, err := os.Stat(questsDir)
	if err != nil {
		if os.IsNotExist(err) {
			return []Quest{}, nil
		}
		return nil, err
	}
	if !info.IsDir() {
		return nil, &os.PathError{Op: "readdir", Path: questsDir, Err: os.ErrInvalid}
	}

	entries, err := os.ReadDir(questsDir)
	if err != nil {
		return nil, err
	}

	quests := make([]Quest, 0, len(entries))
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		if filepath.Ext(entry.Name()) != ".yaml" {
			continue
		}
		path := filepath.Join(questsDir, entry.Name())
		q, err := ParseQuest(path)
		if err != nil {
			return nil, err
		}
		quests = append(quests, *q)
	}
	return quests, nil
}
