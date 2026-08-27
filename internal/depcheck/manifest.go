package depcheck

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"io"
	"strings"
)

// The ecosystem tools (`go list -m -u`, `dotnet list package --outdated`,
// `npm outdated`) read the WORKING TREE, so on a checkout that is behind the
// remote they report a dependency as outdated that upstream has already
// updated — which is what filed duplicate beads for work that was already done,
// and what the old `git pull --ff-only` existed to prevent.
//
// The pull is gone, so the correction is made from the data instead: the
// committed manifests are read out of the tracking ref (never from disk) and
// each reported update is reconciled against what upstream actually pins. The
// parsers below take bytes for exactly that reason — the same function serves a
// blob and a file.

// parseGoModRequires extracts the DIRECT requirements of a go.mod: module path
// to version. Indirect requirements are excluded because depcheck never reports
// them — they cannot be independently upgraded.
func parseGoModRequires(data []byte) map[string]string {
	requires := map[string]string{}
	inBlock := false

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimSpace(raw)
		if idx := strings.Index(line, "//"); idx >= 0 {
			// An `// indirect` marker disqualifies the line entirely.
			if strings.Contains(line[idx:], "indirect") {
				continue
			}
			line = strings.TrimSpace(line[:idx])
		}
		if line == "" {
			continue
		}

		switch {
		case inBlock:
			if line == ")" {
				inBlock = false
				continue
			}
		case line == "require (":
			inBlock = true
			continue
		case strings.HasPrefix(line, "require "):
			line = strings.TrimSpace(strings.TrimPrefix(line, "require "))
		default:
			continue
		}

		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		requires[fields[0]] = fields[1]
	}
	return requires
}

// parsePackageRefs extracts NuGet package pins from an MSBuild file — the
// <PackageReference> of a .csproj and the <PackageVersion> of a central
// Directory.Packages.props. Both spell the package name Include (or Update,
// which central management uses to override one entry) and the version either
// as a Version attribute or a nested <Version> element.
//
// Only a version that names ONE concrete version is recorded — see
// nugetPinnedVersion. Anything else (an MSBuild property reference, an interval,
// a float) is skipped rather than recorded: it cannot be compared against a
// concrete version, and an unrecorded package is passed through untouched, which
// is the direction reconcileWithCommitted's own contract argues for.
func parsePackageRefs(data []byte) map[string]string {
	refs := map[string]string{}
	dec := xml.NewDecoder(bytes.NewReader(data))
	dec.Strict = false

	for {
		tok, err := dec.Token()
		if err != nil {
			if err == io.EOF {
				break
			}
			// Malformed XML past this point: keep whatever parsed cleanly.
			break
		}
		start, ok := tok.(xml.StartElement)
		if !ok {
			continue
		}
		if start.Name.Local != "PackageReference" && start.Name.Local != "PackageVersion" {
			continue
		}

		var elem struct {
			Include string `xml:"Include,attr"`
			Update  string `xml:"Update,attr"`
			Version string `xml:"Version,attr"`
			Nested  string `xml:"Version"`
		}
		if err := dec.DecodeElement(&elem, &start); err != nil {
			continue
		}

		name := elem.Include
		if name == "" {
			name = elem.Update
		}
		version := elem.Version
		if version == "" {
			version = elem.Nested
		}
		version = nugetPinnedVersion(version)
		if name == "" || version == "" {
			continue
		}
		refs[name] = version
	}
	return refs
}

// nugetPinnedVersion reduces a NuGet version expression to the single concrete
// version it pins, or "" when it names no such version.
//
// NuGet spells an exact pin two ways — bare ("1.2.3", nominally a minimum, and
// the form depcheck has always read as the pin) and bracketed ("[1.2.3]") — and
// it also accepts intervals ("[1.0.0,2.0.0)") and floats ("1.0.*"), neither of
// which names a version at all. Recording one of those verbatim was worse than
// recording nothing twice over: reconcileWithCommitted rewrote the reported
// current version to the raw range text, so classifyUpdate re-derived the kind
// from "[1" and reported every such update as `major` (routing a patch bump to
// needs-attention), and the already-at-latest DROP never fired, because
// "[1.2.4]" is not "1.2.4" — so an update upstream had already merged was
// emitted as a no-op `major` from [1.2.4] to 1.2.4, which is the duplicate bead
// this reconciliation exists to prevent.
func nugetPinnedVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" || strings.Contains(v, "$(") {
		return ""
	}
	// [1.2.3] is the bracket spelling of one exact version; every other
	// bracketed or parenthesised form is an interval with two ends.
	if strings.HasPrefix(v, "[") && strings.HasSuffix(v, "]") {
		v = strings.TrimSpace(v[1 : len(v)-1])
	}
	if strings.ContainsAny(v, "[](),*") {
		return ""
	}
	return v
}

// parsePackageJSONDeps extracts the declared dependency ranges of a
// package.json. The values are ranges ("^1.2.3"), not resolved versions, which
// is why an npm reconcile only ever drops an entry and never rewrites its
// reported current version.
func parsePackageJSONDeps(data []byte) map[string]string {
	var pkg struct {
		Dependencies         map[string]string `json:"dependencies"`
		DevDependencies      map[string]string `json:"devDependencies"`
		OptionalDependencies map[string]string `json:"optionalDependencies"`
	}
	if err := json.Unmarshal(data, &pkg); err != nil {
		return nil
	}

	deps := map[string]string{}
	for _, set := range []map[string]string{pkg.Dependencies, pkg.DevDependencies, pkg.OptionalDependencies} {
		for name, version := range set {
			deps[name] = version
		}
	}
	return deps
}

// normalizeVersion reduces a manifest's version expression to the bare version
// it pins: "^1.2.3" and "v1.2.3" and ">=1.2.3 <2.0.0" all become "1.2.3". It is
// only ever used to compare a manifest entry against a concrete version the
// ecosystem tool reported, never to order two versions.
//
// A BARE strict inequality names the one version its range excludes, so it
// normalizes to "" rather than to that version: "<2.0.0" reduced to "2.0.0" made
// reconcileWithCommitted read a manifest that genuinely needs bumping as one
// upstream had already bumped, and drop a real major update with no trace. "" is
// the same answer as a package the manifests do not mention — passed through
// untouched. A compound range is unaffected either way, since only its first
// field is read, and "<=1.2.3" does admit 1.2.3 and so keeps it.
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if fields := strings.Fields(v); len(fields) > 0 {
		v = fields[0]
	}
	switch {
	case strings.HasPrefix(v, "<="), strings.HasPrefix(v, ">="):
		v = v[2:]
	case strings.HasPrefix(v, "<"), strings.HasPrefix(v, ">"):
		return ""
	}
	v = strings.TrimLeft(v, "^~=v ")
	return strings.TrimSpace(v)
}

// reconcileWithCommitted corrects updates reported off a working tree against
// what the tracking ref actually pins.
//
// An entry the committed manifest already pins at the latest version is
// dropped: upstream has done that upgrade, and reporting it would file a bead
// for work that is already merged — the duplicate-bead failure the removed
// `git pull` was there to prevent.
//
// When exact is true the manifest holds a resolved version (go.mod, a NuGet
// PackageReference), so a stale reported version is replaced by the committed
// one and the update reclassified. When it is false the manifest holds a range
// (package.json), which says nothing about what is installed, so the reported
// version is left alone.
//
// A package the manifests do not mention is passed through untouched: absence
// means the committed state could not be established, and dropping on that
// would silently shrink the scan.
func reconcileWithCommitted(updates []ModuleUpdate, committed map[string]string, exact bool) []ModuleUpdate {
	if len(committed) == 0 {
		return updates
	}

	kept := make([]ModuleUpdate, 0, len(updates))
	for _, u := range updates {
		pinned, ok := committed[u.Path]
		if !ok {
			kept = append(kept, u)
			continue
		}
		normalized := normalizeVersion(pinned)
		if normalized == "" {
			kept = append(kept, u)
			continue
		}
		if normalized == normalizeVersion(u.Latest) {
			continue // already updated upstream
		}
		if exact && normalized != normalizeVersion(u.Current) {
			u.Current = pinned
			u.Kind = classifyUpdate(pinned, u.Latest)
		}
		kept = append(kept, u)
	}
	return kept
}
