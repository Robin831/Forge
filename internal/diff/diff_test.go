package diff

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestChangedFiles(t *testing.T) {
	d := "diff --git a/foo.cs b/foo.cs\n@@ -1 +1 @@\n-old\n+new\n" +
		"diff --git a/bar/baz.tsx b/bar/baz.tsx\n@@ -1 +1 @@\n-x\n+y\n"
	got := ChangedFiles(d)
	assert.Equal(t, []string{"foo.cs", "bar/baz.tsx"}, got)
}

func TestChangedFiles_NoHeaders(t *testing.T) {
	assert.Nil(t, ChangedFiles("no headers here"))
	assert.Nil(t, ChangedFiles(""))
}

func TestParseGitPath(t *testing.T) {
	assert.Equal(t, "foo.cs", ParseGitPath("diff --git a/foo.cs b/foo.cs"))
	assert.Equal(t, "bar/baz.tsx", ParseGitPath("diff --git a/bar/baz.tsx b/bar/baz.tsx"))
	// Renames show the new (b-side) path.
	assert.Equal(t, "new/path.go", ParseGitPath("diff --git a/old/path.go b/new/path.go"))
	// Non-header lines return "".
	assert.Equal(t, "", ParseGitPath("@@ -1 +1 @@"))
	assert.Equal(t, "", ParseGitPath(""))
}

func TestTruncate(t *testing.T) {
	// Short diffs are returned unchanged.
	assert.Equal(t, "small", Truncate("small", 100))

	// Diffs over the cap are truncated and annotated with the omitted byte count.
	big := strings.Repeat("x", 50)
	out := Truncate(big, 10)
	assert.True(t, strings.HasPrefix(out, strings.Repeat("x", 10)))
	assert.Contains(t, out, "diff truncated, 40 bytes omitted")
}
