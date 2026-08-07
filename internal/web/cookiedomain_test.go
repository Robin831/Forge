package web

import "testing"

// sharedCookieDomain decides how far the Hearth session cookie is handed out,
// so every case here is either "the deployment works" or "a session is not
// given to hosts this deployment does not own".
func TestSharedCookieDomain(t *testing.T) {
	cases := []struct {
		name       string
		hearthHost string
		proxyBase  string
		want       string
		wantOK     bool
	}{
		{
			name:       "sibling subdomains share their parent",
			hearthHost: "hearth.example.com",
			proxyBase:  "preview.example.com",
			want:       "example.com",
			wantOK:     true,
		},
		{
			name:       "base under the hearth host shares the hearth host",
			hearthHost: "example.com",
			proxyBase:  "preview.example.com",
			want:       "example.com",
			wantOK:     true,
		},
		{
			name:       "the deepest shared parent wins, not the registrable one",
			hearthHost: "hearth.team.example.co.uk",
			proxyBase:  "preview.team.example.co.uk",
			want:       "team.example.co.uk",
			wantOK:     true,
		},
		{
			name:       "identical hosts share the whole name",
			hearthHost: "forge.example.com",
			proxyBase:  "forge.example.com",
			want:       "forge.example.com",
			wantOK:     true,
		},
		{
			name:       "ports and case and a trailing root dot are ignored",
			hearthHost: "Hearth.Example.COM.:8080",
			proxyBase:  "PREVIEW.example.com.",
			want:       "example.com",
			wantOK:     true,
		},
		{
			name:       "different registrable domains share nothing",
			hearthHost: "hearth.example.com",
			proxyBase:  "preview.other.com",
			wantOK:     false,
		},
		{
			// The supercookie case: github.io is a public suffix, so a Domain
			// there would be a grant to every unrelated GitHub Pages site.
			name:       "a public suffix is not a shared parent",
			hearthHost: "a.github.io",
			proxyBase:  "b.github.io",
			wantOK:     false,
		},
		{
			name:       "an unknown TLD counts as a public suffix too",
			hearthHost: "hearth.test",
			proxyBase:  "preview.test",
			wantOK:     false,
		},
		{
			name:       "but a name under that TLD is registrable",
			hearthHost: "hearth.preview.test",
			proxyBase:  "preview.test",
			want:       "preview.test",
			wantOK:     true,
		},
		{
			name:       "localhost has no registrable parent",
			hearthHost: "localhost",
			proxyBase:  "preview.localhost",
			wantOK:     false,
		},
		{
			name:       "an IPv4 hearth host cannot carry a Domain",
			hearthHost: "127.0.0.1:8080",
			proxyBase:  "preview.example.com",
			wantOK:     false,
		},
		{
			name:       "an IPv6 hearth host cannot either",
			hearthHost: "[::1]:8080",
			proxyBase:  "preview.example.com",
			wantOK:     false,
		},
		{
			name:       "an empty base means host-based routing is off",
			hearthHost: "hearth.example.com",
			wantOK:     false,
		},
		{
			name:      "an empty hearth host tells us nothing",
			proxyBase: "preview.example.com",
			wantOK:    false,
		},
		{
			// "hearthexample.com" ends with "example.com" as a *string* but not
			// on a label boundary — a suffix match that ignored that would be a
			// grant to a domain somebody else registered.
			name:       "the suffix must align on a label boundary",
			hearthHost: "notexample.com",
			proxyBase:  "preview.example.com",
			wantOK:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := sharedCookieDomain(tc.hearthHost, tc.proxyBase)
			if ok != tc.wantOK {
				t.Fatalf("sharedCookieDomain(%q, %q) ok = %v, want %v (domain %q)",
					tc.hearthHost, tc.proxyBase, ok, tc.wantOK, got)
			}
			if ok && got != tc.want {
				t.Fatalf("sharedCookieDomain(%q, %q) = %q, want %q",
					tc.hearthHost, tc.proxyBase, got, tc.want)
			}
			if !ok && got != "" {
				t.Fatalf("refused domain must be empty, got %q", got)
			}
		})
	}
}
