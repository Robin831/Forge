package questgiver

import (
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
		return nil, err
	}
	var q Quest
	if err := yaml.Unmarshal(data, &q); err != nil {
		return nil, err
	}
	q.FilePath = path
	return &q, nil
}

// DiscoverQuests finds all quest YAML files under <anvilPath>/.forge/quests/
// and returns the parsed quests. Returns an empty slice if the directory does not exist.
func DiscoverQuests(anvilPath string) ([]Quest, error) {
	pattern := filepath.Join(anvilPath, ".forge", "quests", "*.yaml")
	matches, err := filepath.Glob(pattern)
	if err != nil {
		return nil, err
	}
	quests := make([]Quest, 0, len(matches))
	for _, m := range matches {
		q, err := ParseQuest(m)
		if err != nil {
			return nil, err
		}
		quests = append(quests, *q)
	}
	return quests, nil
}
