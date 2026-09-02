package warden

import (
	"testing"

	"github.com/bmatcuk/doublestar/v4"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestDerivePaths(t *testing.T) {
	cases := []struct {
		name  string
		files []string
		want  []string
	}{
		{
			name:  "code-only diff in one area",
			files: []string{"api/Controllers/Orders.cs", "api/Services/Pricing.cs"},
			want:  []string{"api/**/*.cs"},
		},
		{
			name:  "multi-area diff yields one scoped glob per area",
			files: []string{"api/Orders.cs", "worker/Job.cs"},
			want:  []string{"api/**/*.cs", "worker/**/*.cs"},
		},
		{
			name:  "single file yields its own area and nothing wider",
			files: []string{"api/Controllers/Orders.cs"},
			want:  []string{"api/**/*.cs"},
		},
		{
			name:  "a doc glob appears only when the diff carries a doc file",
			files: []string{"api/Orders.cs", "docs/api.md"},
			want:  []string{"api/**/*.cs", "docs/**/*.md"},
		},
		{
			name:  "a file in the repository root is scoped to the root",
			files: []string{"main.go", "internal/daemon/poll.go"},
			want:  []string{"*.go", "internal/**/*.go"},
		},
		{
			name:  "extensionless files contribute nothing",
			files: []string{"Makefile", "Dockerfile", "LICENSE"},
			want:  nil,
		},
		{
			name:  "no files at all",
			files: nil,
			want:  nil,
		},
		{
			name:  "paths are normalized before the area is read",
			files: []string{"./api/Orders.cs", `api\Services\Pricing.cs`},
			want:  []string{"api/**/*.cs"},
		},
		{
			name:  "one glob per (area, extension) pair, deduplicated and sorted",
			files: []string{"web/b.ts", "web/a.ts", "cmd/forge/main.go", "web/App.tsx"},
			want:  []string{"cmd/**/*.go", "web/**/*.ts", "web/**/*.tsx"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, DerivePaths(tc.files))
		})
	}
}

// The acceptance criterion, stated as the two things the derivation must never
// produce: the bare language glob that fires in every directory of the
// repository, and a documentation glob on a diff that changed no documentation.
func TestDerivePathsNeverEmitsRepoWideOrUnevidencedGlobs(t *testing.T) {
	got := DerivePaths([]string{
		"api/Controllers/Orders.cs",
		"api/Services/Pricing.cs",
		"worker/Jobs/Nightly.cs",
	})
	require.NotEmpty(t, got)
	assert.Equal(t, []string{"api/**/*.cs", "worker/**/*.cs"}, got)
	for _, g := range got {
		assert.NotEqual(t, "**/*.cs", g, "a bare language glob names no location")
		assert.NotEqual(t, "**/*.md", g)
		assert.NotEqual(t, "**/*", g)
		assert.NotEqual(t, "**", g)
	}
}

// Every derived glob has to be one warden.FilterRules can actually fire on, or
// the rule is gated on something that matches nothing and looks exactly like a
// rule with nothing to say.
func TestDerivedPathsMatchTheFilesTheyCameFrom(t *testing.T) {
	files := []string{
		"api/Controllers/Orders.cs",
		"worker/Job.cs",
		"docs/api.md",
		"main.go",
	}
	globs := DerivePaths(files)
	require.NotEmpty(t, globs)

	for _, g := range globs {
		var matched bool
		for _, f := range files {
			ok, err := doublestar.Match(g, f)
			require.NoError(t, err, g)
			matched = matched || ok
		}
		assert.True(t, matched, "%s matches none of the files it was derived from", g)
	}

	// And the scoping really scopes: a sibling area's file of the same
	// extension is not matched by the area it does not live in.
	ok, err := doublestar.Match("api/**/*.cs", "worker/Job.cs")
	require.NoError(t, err)
	assert.False(t, ok)
}

func TestScopeExtGlobs(t *testing.T) {
	files := []string{"internal/daemon/poll.go", "web/src/App.tsx", "changelog.d/x.md"}

	t.Run("scopes a bare extension glob to the areas the files occupy", func(t *testing.T) {
		assert.Equal(t, []string{"internal/**/*.go"}, ScopeExtGlobs([]string{"**/*.go"}, files))
	})

	t.Run("passes a directory glob through untouched", func(t *testing.T) {
		assert.Equal(t, []string{"changelog.d/**"}, ScopeExtGlobs([]string{"changelog.d/**"}, files))
	})

	t.Run("keeps a bare glob the files carry no evidence for", func(t *testing.T) {
		// No .cs file, so there is no area to place the glob in. Dropping it
		// would leave the rule gated on nothing at all, which is wider than
		// the glob being scoped, not narrower.
		assert.Equal(t, []string{"**/*.cs"}, ScopeExtGlobs([]string{"**/*.cs"}, files))
	})

	t.Run("an already scoped glob is left alone", func(t *testing.T) {
		assert.Equal(t, []string{"cmd/**/*.go"}, ScopeExtGlobs([]string{"cmd/**/*.go"}, files))
	})

	t.Run("empty inputs", func(t *testing.T) {
		assert.Nil(t, ScopeExtGlobs(nil, files))
		assert.Equal(t, []string{"**/*.go"}, ScopeExtGlobs([]string{"**/*.go"}, nil))
	})
}

func TestBaseGlob(t *testing.T) {
	for _, tc := range []struct{ glob, want string }{
		{"api/**/*.cs", "**/*.cs"},
		{"internal/daemon/**/*.go", "**/*.go"},
		{"*.go", "**/*.go"},
		{"**/*.go", "**/*.go"},
		{"changelog.d/**", "changelog.d/**"},
		{"**/*", "**/*"},
		{"**", "**"},
		// Not a shape this derivation emits, and `**/testdata/*.json` does
		// restrict where it matches — so it is left exactly as it is rather
		// than widened into a claim about the whole repository.
		{"**/testdata/*.json", "**/testdata/*.json"},
		{"docs/configuration.md", "docs/configuration.md"},
	} {
		assert.Equal(t, tc.want, BaseGlob(tc.glob), tc.glob)
	}
}

// The containment BaseGlob asserts, checked against doublestar rather than
// assumed: every path a scoped glob matches must be matched by its base, or the
// smelter's coverage test would call a widening a narrowing.
func TestBaseGlobIsAContainment(t *testing.T) {
	paths := []string{
		"Orders.cs",
		"api/Orders.cs",
		"api/Controllers/Orders.cs",
		"worker/Job.cs",
	}
	for _, glob := range []string{"api/**/*.cs", "*.cs", "api/Controllers/**/*.cs"} {
		base := BaseGlob(glob)
		for _, p := range paths {
			narrow, err := doublestar.Match(glob, p)
			require.NoError(t, err)
			if !narrow {
				continue
			}
			broad, err := doublestar.Match(base, p)
			require.NoError(t, err)
			assert.True(t, broad, "%s matches %s but its base %s does not", glob, p, base)
		}
	}
}

func TestExtGlobs(t *testing.T) {
	assert.Equal(t, []string{"**/*.go", "**/*.md", "**/*.ts"},
		ExtGlobs([]string{"cmd/forge/main.go", "web/src/app.ts", "web/src/Foo.ts", "docs/README.md"}))
	assert.Nil(t, ExtGlobs(nil))
	assert.Nil(t, ExtGlobs([]string{"Makefile", "Dockerfile"}))
	assert.Nil(t, ExtGlobs([]string{"foo."}), "a trailing dot is a degenerate extension")
	assert.Equal(t, []string{"**/*.gitignore"}, ExtGlobs([]string{".gitignore"}))
}
