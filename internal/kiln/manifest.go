// Package kiln implements Kiln — on-demand preview environments for worker
// branches (see docs/plans/preview-environments.md).
//
// This file covers the declarative half of the feature: the per-project
// preview manifest (`<anvil>/.forge/preview.yaml`), its loader, and its
// validation rules. Process supervision, port allocation and health checking
// arrive in later phases; nothing here starts or touches a process.
package kiln

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

const (
	// ManifestVersion is the only manifest schema version Kiln understands.
	// A manifest omitting `version` is treated as this version.
	ManifestVersion = 1

	// ManifestFile is the manifest's file name inside an anvil's .forge dir.
	ManifestFile = "preview.yaml"

	// DefaultReadyTimeout is applied to a service that does not declare its
	// own `ready_timeout`.
	DefaultReadyTimeout = 60 * time.Second

	// MinReadyTimeout is the smallest accepted explicit `ready_timeout`. It
	// exists to catch `ready_timeout: 120` (bare numbers are nanoseconds in
	// YAML duration decoding) rather than to express a real lower bound.
	MinReadyTimeout = time.Second
)

// ErrNoManifest is returned (wrapped) by Load when the anvil has no
// `.forge/preview.yaml`. Callers use errors.Is to treat a preview-less anvil
// as normal rather than as a failure — such an anvil simply offers no preview.
var ErrNoManifest = errors.New("no preview manifest")

// Manifest is a parsed `.forge/preview.yaml`: how one project starts under a
// preview. It is always read from the anvil's MAIN checkout, never from the
// PR branch — see Load.
type Manifest struct {
	// Version is the schema version. Only ManifestVersion is supported; an
	// omitted version is normalized to it.
	Version int `yaml:"version"`
	// Setup runs once before any service starts (e.g. create + migrate a
	// per-preview database). Optional.
	Setup string `yaml:"setup"`
	// Teardown runs once after every service has stopped (e.g. drop the
	// per-preview database). Optional.
	Teardown string `yaml:"teardown"`
	// Services are the processes that make up the preview, in manifest order.
	Services Services `yaml:"services"`

	// Path is the file this manifest was read from. Empty when parsed from
	// bytes. Not part of the YAML schema.
	Path string `yaml:"-"`
}

// Service is one supervised process in a preview.
type Service struct {
	// Name is the manifest's map key for this service. Not a YAML field.
	Name string `yaml:"-"`
	// Command is the command line to run, relative to Dir. Required.
	Command string `yaml:"command"`
	// Dir is the working directory relative to the preview worktree root.
	// Empty means the worktree root.
	Dir string `yaml:"dir"`
	// Env holds extra environment variables for this service, layered on top
	// of the FORGE_* context variables Kiln injects.
	Env map[string]string `yaml:"env"`
	// Health is an HTTP path (e.g. "/healthz") probed on the service's
	// allocated port. Empty means "port is open" is the readiness signal.
	Health string `yaml:"health"`
	// ReadyTimeout bounds how long the readiness check may take before the
	// service is considered failed. Zero means DefaultReadyTimeout.
	ReadyTimeout time.Duration `yaml:"ready_timeout"`
	// Entry marks the service whose URL is *the* preview link. Required when
	// the manifest declares more than one service; implicit for a lone one.
	Entry bool `yaml:"entry"`
}

// Services is the manifest's `services:` mapping, decoded into a slice so
// manifest order (and therefore start order) is preserved.
type Services []Service

// serviceFields is the accepted key set for a service mapping. Services has a
// custom unmarshaler, which bypasses the decoder's KnownFields strictness, so
// unknown keys are rejected here instead.
var serviceFields = []string{"command", "dir", "entry", "env", "health", "ready_timeout"}

// serviceNamePattern bounds service names to characters that are safe in an
// environment variable suffix (FORGE_PREVIEW_PORT_<NAME>), a log file name and
// a URL host label.
var serviceNamePattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]*$`)

// UnmarshalYAML decodes the `services:` mapping while preserving key order,
// rejecting duplicate service names, and rejecting unknown per-service fields.
func (s *Services) UnmarshalYAML(node *yaml.Node) error {
	if node.Kind != yaml.MappingNode {
		return fmt.Errorf("services: expected a mapping of service name to definition, got %s", nodeKind(node))
	}
	out := make(Services, 0, len(node.Content)/2)
	seen := make(map[string]int, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		keyNode, valNode := node.Content[i], node.Content[i+1]
		var name string
		if err := keyNode.Decode(&name); err != nil {
			return fmt.Errorf("services: line %d: service name must be a string", keyNode.Line)
		}
		if prev, dup := seen[name]; dup {
			return fmt.Errorf("services: duplicate service %q (line %d, first defined on line %d)", name, keyNode.Line, prev)
		}
		seen[name] = keyNode.Line
		if valNode.Kind != yaml.MappingNode {
			return fmt.Errorf("service %q: expected a mapping of fields, got %s", name, nodeKind(valNode))
		}
		for j := 0; j+1 < len(valNode.Content); j += 2 {
			var field string
			if err := valNode.Content[j].Decode(&field); err != nil {
				return fmt.Errorf("service %q: line %d: field name must be a string", name, valNode.Content[j].Line)
			}
			if !containsString(serviceFields, field) {
				return fmt.Errorf("service %q: unknown field %q (known fields: %s)", name, field, strings.Join(serviceFields, ", "))
			}
			// A bare number is the natural way to get ready_timeout wrong;
			// answer it with the fix rather than a YAML type error.
			if field == "ready_timeout" && valNode.Content[j+1].Tag == "!!int" {
				return fmt.Errorf("service %q: ready_timeout must be a duration string such as %q (got the bare number %s)",
					name, "120s", valNode.Content[j+1].Value)
			}
		}
		var svc Service
		if err := valNode.Decode(&svc); err != nil {
			return fmt.Errorf("service %q: %w", name, err)
		}
		svc.Name = name
		out = append(out, svc)
	}
	*s = out
	return nil
}

// Get returns the named service.
func (s Services) Get(name string) (Service, bool) {
	for _, svc := range s {
		if svc.Name == name {
			return svc, true
		}
	}
	return Service{}, false
}

// Names returns the service names in manifest order.
func (s Services) Names() []string {
	out := make([]string, 0, len(s))
	for _, svc := range s {
		out = append(out, svc.Name)
	}
	return out
}

// Entry returns the service whose URL is the preview link.
func (m *Manifest) Entry() (Service, bool) {
	for _, svc := range m.Services {
		if svc.Entry {
			return svc, true
		}
	}
	if len(m.Services) == 1 {
		return m.Services[0], true
	}
	return Service{}, false
}

// ManifestPath returns the manifest path for an anvil checkout.
func ManifestPath(anvilPath string) string {
	return filepath.Join(anvilPath, ".forge", ManifestFile)
}

// Exists reports whether the anvil checkout declares a preview manifest. It is
// the cheap gate for "does this anvil offer previews at all" and never parses.
func Exists(anvilPath string) bool {
	info, err := os.Stat(ManifestPath(anvilPath))
	return err == nil && !info.IsDir()
}

// Load reads, parses and validates the preview manifest of an anvil.
//
// anvilMainPath must be the anvil's MAIN checkout, never a worker/preview
// worktree of the PR branch: the manifest decides which commands Kiln executes
// on the host, so a PR must not be able to change it. The consequence is that
// a PR which changes how the app starts is only previewable once its manifest
// change has landed on the main branch.
//
// When the anvil has no manifest, the returned error wraps ErrNoManifest so
// callers can distinguish "no previews here" from a broken manifest.
func Load(anvilMainPath string) (*Manifest, error) {
	path := ManifestPath(anvilMainPath)
	data, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, fmt.Errorf("kiln: %s: %w", path, ErrNoManifest)
		}
		return nil, fmt.Errorf("kiln: reading %s: %w", path, err)
	}
	m, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("kiln: %s: %w", path, err)
	}
	m.Path = path
	return m, nil
}

// Parse parses and validates a manifest from raw YAML bytes.
func Parse(data []byte) (*Manifest, error) {
	m, err := parse(data)
	if err != nil {
		return nil, fmt.Errorf("kiln: %s: %w", ManifestFile, err)
	}
	return m, nil
}

// parse does the work behind Parse and Load, returning unprefixed errors so
// each caller can attach the location it knows about (file path vs file name).
func parse(data []byte) (*Manifest, error) {
	var m Manifest
	dec := yaml.NewDecoder(bytes.NewReader(data))
	dec.KnownFields(true)
	if err := dec.Decode(&m); err != nil {
		if errors.Is(err, io.EOF) {
			return nil, errors.New("manifest is empty")
		}
		return nil, err
	}
	if err := m.Validate(); err != nil {
		return nil, err
	}
	m.normalize()
	return &m, nil
}

// Validate reports the first problem that would make this manifest unusable.
// Every error names the offending service and field so the operator can fix
// the YAML without guessing.
func (m *Manifest) Validate() error {
	if m.Version != 0 && m.Version != ManifestVersion {
		return fmt.Errorf("unsupported version %d (supported: %d)", m.Version, ManifestVersion)
	}
	if len(m.Services) == 0 {
		return errors.New("no services declared (at least one service is required)")
	}

	entries := 0
	// Service names fold to a single env var each (FORGE_PREVIEW_PORT_<NAME>),
	// so two names that differ only in case or in '-' vs '_' would silently
	// share one port variable. Reject that rather than start a preview where
	// one service is told the other's port.
	portVars := make(map[string]string, len(m.Services))
	for _, svc := range m.Services {
		if err := svc.validate(); err != nil {
			return err
		}
		portVar := PortEnvVar(svc.Name)
		if prev, dup := portVars[portVar]; dup {
			return fmt.Errorf("services %q and %q both map to %s — service names must differ by more than case or %q vs %q",
				prev, svc.Name, portVar, "-", "_")
		}
		portVars[portVar] = svc.Name
		if svc.Entry {
			entries++
		}
	}
	switch {
	case entries > 1:
		return fmt.Errorf("multiple services marked entry: true (%s) — exactly one service is the preview link",
			strings.Join(entryNames(m.Services), ", "))
	case entries == 0 && len(m.Services) > 1:
		return fmt.Errorf("no service marked entry: true — one of %s must set entry: true when the manifest declares more than one service",
			strings.Join(m.Services.Names(), ", "))
	}

	return m.validateTemplates()
}

// validate checks a single service's fields.
func (s Service) validate() error {
	if strings.TrimSpace(s.Name) == "" {
		return errors.New("service with an empty name")
	}
	if !serviceNamePattern.MatchString(s.Name) {
		return fmt.Errorf("service %q: name must match %s (it becomes an env var suffix and a log file name)", s.Name, serviceNamePattern)
	}
	if strings.TrimSpace(s.Command) == "" {
		return fmt.Errorf("service %q: command is required", s.Name)
	}
	if err := validateDir(s.Name, s.Dir); err != nil {
		return err
	}
	if s.Health != "" && !strings.HasPrefix(s.Health, "/") {
		return fmt.Errorf("service %q: health must be a path starting with %q (got %q)", s.Name, "/", s.Health)
	}
	switch {
	case s.ReadyTimeout < 0:
		return fmt.Errorf("service %q: ready_timeout must not be negative", s.Name)
	case s.ReadyTimeout > 0 && s.ReadyTimeout < MinReadyTimeout:
		return fmt.Errorf("service %q: ready_timeout must be at least %s — write it as a duration string such as \"120s\" (a bare number is nanoseconds)", s.Name, MinReadyTimeout)
	}
	for key := range s.Env {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("service %q: env has an empty variable name", s.Name)
		}
	}
	return nil
}

// validateDir keeps a service's working directory inside the preview worktree.
func validateDir(service, dir string) error {
	if dir == "" {
		return nil
	}
	if filepath.IsAbs(dir) || strings.HasPrefix(dir, "/") || strings.HasPrefix(dir, `\`) {
		return fmt.Errorf("service %q: dir must be relative to the preview worktree (got %q)", service, dir)
	}
	clean := filepath.ToSlash(filepath.Clean(dir))
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return fmt.Errorf("service %q: dir must stay inside the preview worktree (got %q)", service, dir)
	}
	return nil
}

// validateTemplates catches template syntax errors and references to services
// that do not exist, at load time rather than when a preview is started. It
// runs the real expansion against probe values and discards the result.
func (m *Manifest) validateTemplates() error {
	ports := make(map[string]int, len(m.Services))
	for _, svc := range m.Services {
		ports[svc.Name] = probePort
	}
	_, err := m.Expand(Context{PreviewID: "probe", Host: "127.0.0.1", Ports: ports})
	return err
}

// normalize fills in the defaults the rest of Kiln relies on: an implicit
// version, an implicit entry service when there is only one, and the default
// ready timeout. It runs after validation so validation always sees what the
// operator actually wrote.
func (m *Manifest) normalize() {
	if m.Version == 0 {
		m.Version = ManifestVersion
	}
	if len(m.Services) == 1 {
		m.Services[0].Entry = true
	}
	for i := range m.Services {
		if m.Services[i].ReadyTimeout == 0 {
			m.Services[i].ReadyTimeout = DefaultReadyTimeout
		}
	}
}

// entryNames lists the services that set entry: true.
func entryNames(services Services) []string {
	var out []string
	for _, svc := range services {
		if svc.Entry {
			out = append(out, svc.Name)
		}
	}
	sort.Strings(out)
	return out
}

func containsString(haystack []string, needle string) bool {
	for _, s := range haystack {
		if s == needle {
			return true
		}
	}
	return false
}

// nodeKind renders a YAML node kind for error messages.
func nodeKind(node *yaml.Node) string {
	switch node.Kind {
	case yaml.SequenceNode:
		return "a list"
	case yaml.ScalarNode:
		return "a scalar"
	case yaml.AliasNode:
		return "an alias"
	case yaml.DocumentNode:
		return "a document"
	case yaml.MappingNode:
		return "a mapping"
	default:
		return "an unknown node"
	}
}
