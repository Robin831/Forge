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
// A version that is an MSBuild property reference ($(Foo)) is skipped rather
// than recorded: it cannot be compared against a concrete version, and
// recording it would make an update look already-applied.
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
			version = strings.TrimSpace(elem.Nested)
		}
		if name == "" || version == "" || strings.Contains(version, "$(") {
			continue
		}
		refs[name] = version
	}
	return refs
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
func normalizeVersion(v string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return ""
	}
	if fields := strings.Fields(v); len(fields) > 0 {
		v = fields[0]
	}
	v = strings.TrimLeft(v, "^~>=<v ")
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
