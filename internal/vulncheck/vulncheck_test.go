package vulncheck

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseGovulncheckJSON(t *testing.T) {
	// Simulate govulncheck -json output (newline-delimited JSON)
	input := `{"osv":{"id":"GO-2024-0001","summary":"Buffer overflow in example.com/foo","details":"A buffer overflow exists...","affected":[{"package":{"name":"example.com/foo","ecosystem":"Go"},"ranges":[{"events":[{"introduced":"0"},{"fixed":"1.2.3"}]}]}],"database_specific":{"severity":"HIGH"}}}
{"finding":{"osv":"GO-2024-0001","trace":[{"module":"example.com/foo","version":"v1.1.0","package":"example.com/foo/bar"}]}}
{"osv":{"id":"GO-2024-0002","summary":"SQL injection in example.com/db","details":"SQL injection vulnerability...","affected":[{"package":{"name":"example.com/db","ecosystem":"Go"},"ranges":[{"events":[{"introduced":"0"},{"fixed":"2.0.0"}]}]}],"database_specific":{"severity":"CRITICAL"}}}
{"finding":{"osv":"GO-2024-0002","trace":[{"module":"example.com/db","version":"v1.5.0","package":"example.com/db/query"}]}}
{"osv":{"id":"GO-2024-0003","summary":"Info disclosure (not called)","details":"Not actually used in code","affected":[{"package":{"name":"example.com/unused","ecosystem":"Go"},"ranges":[{"events":[{"introduced":"0"},{"fixed":"3.0.0"}]}]}]}}
`

	vulns, err := parseGovulncheckJSON([]byte(input))
	require.NoError(t, err)
	assert.Len(t, vulns, 2, "should only include vulns with findings")

	// Build a map for easier assertion
	byID := make(map[string]ParsedVuln)
	for _, v := range vulns {
		byID[v.ID] = v
	}

	// Check HIGH severity vuln
	v1, ok := byID["GO-2024-0001"]
	require.True(t, ok)
	assert.Equal(t, "HIGH", v1.Severity)
	assert.Equal(t, "example.com/foo", v1.AffectedPkg)
	assert.Equal(t, "1.2.3", v1.FixedIn)
	assert.Contains(t, v1.Summary, "Buffer overflow")

	// Check CRITICAL severity vuln
	v2, ok := byID["GO-2024-0002"]
	require.True(t, ok)
	assert.Equal(t, "CRITICAL", v2.Severity)
	assert.Equal(t, "example.com/db", v2.AffectedPkg)
	assert.Equal(t, "2.0.0", v2.FixedIn)

	// GO-2024-0003 should NOT be included (no finding)
	_, ok = byID["GO-2024-0003"]
	assert.False(t, ok, "vuln without finding should be excluded")
}

func TestParseGovulncheckJSON_Empty(t *testing.T) {
	vulns, err := parseGovulncheckJSON([]byte(""))
	require.NoError(t, err)
	assert.Empty(t, vulns)
}

func TestParseGovulncheckJSON_NoFindings(t *testing.T) {
	input := `{"osv":{"id":"GO-2024-0001","summary":"Not called","details":"...","affected":[]}}
`
	vulns, err := parseGovulncheckJSON([]byte(input))
	require.NoError(t, err)
	assert.Empty(t, vulns, "no findings means no vulns reported")
}

func TestSeverityToPriority(t *testing.T) {
	tests := []struct {
		severity string
		want     int
	}{
		{"CRITICAL", 1},
		{"HIGH", 2},
		{"MEDIUM", 3},
		{"LOW", 4},
		{"critical", 1},
		{"high", 2},
		{"unknown", 3},
		{"", 3},
	}

	for _, tt := range tests {
		t.Run(tt.severity, func(t *testing.T) {
			assert.Equal(t, tt.want, severityToPriority(tt.severity))
		})
	}
}

func TestBuildBeadDescription(t *testing.T) {
	v := ParsedVuln{
		ID:          "GO-2024-0001",
		CVEs:        []string{"CVE-2024-1234"},
		Summary:     "Buffer overflow in foo",
		Details:     "A buffer overflow exists in foo/bar",
		Severity:    "HIGH",
		AffectedPkg: "example.com/foo",
		FixedIn:     "1.2.3",
		Symbols:     []string{"DoStuff", "HandleRequest"},
	}

	desc := buildBeadDescription(v, "myrepo")

	assert.Contains(t, desc, "GO-2024-0001")
	assert.Contains(t, desc, "CVE-2024-1234")
	assert.Contains(t, desc, "HIGH")
	assert.Contains(t, desc, "example.com/foo")
	assert.Contains(t, desc, "1.2.3")
	assert.Contains(t, desc, "DoStuff")
	assert.Contains(t, desc, "go get example.com/foo@v1.2.3")
	assert.Contains(t, desc, "govulncheck")
}

func TestBuildBeadDescription_NoFix(t *testing.T) {
	v := ParsedVuln{
		ID:       "GO-2024-0099",
		Summary:  "Something bad",
		Severity: "MEDIUM",
	}

	desc := buildBeadDescription(v, "anvil1")
	assert.Contains(t, desc, "No fix version")
	assert.Contains(t, desc, "alternative packages")
}

func TestTruncate(t *testing.T) {
	assert.Equal(t, "hello", truncate("hello", 10))
	assert.Equal(t, "hel...", truncate("hello world", 6))
	assert.Equal(t, "hello world", truncate("hello world", 100))
}
