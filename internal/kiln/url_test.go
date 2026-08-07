package kiln

import "testing"

func TestEntryURL(t *testing.T) {
	tests := []struct {
		name string
		opts EntryURLOptions
		want string
	}{
		{
			name: "no proxy base: the port the entry service binds",
			opts: EntryURLOptions{BeadID: "Forge-abc1", Host: "forge.wg", Port: 42002},
			want: "http://forge.wg:42002/",
		},
		{
			name: "no proxy base and no port: no link",
			opts: EntryURLOptions{BeadID: "Forge-abc1", Host: "forge.wg"},
			want: "",
		},
		{
			name: "no proxy base and no host: no link",
			opts: EntryURLOptions{BeadID: "Forge-abc1", Port: 42002},
			want: "",
		},
		{
			name: "an IPv6 host is bracketed",
			opts: EntryURLOptions{BeadID: "Forge-abc1", Host: "::1", Port: 42002},
			want: "http://[::1]:42002/",
		},
		{
			name: "proxy base: the preview hostname wins over the port",
			opts: EntryURLOptions{
				BeadID: "Forge-abc1", ProxyBase: "preview.example.com",
				Host: "forge.wg", Port: 42002,
			},
			want: "https://forge-abc1.preview.example.com/",
		},
		{
			name: "proxy base: the bead id is folded to its host label",
			opts: EntryURLOptions{BeadID: "Forge_ABC1", ProxyBase: "preview.example.com"},
			want: "https://forge-abc1.preview.example.com/",
		},
		{
			name: "proxy base: case and a trailing root dot are normalized",
			opts: EntryURLOptions{BeadID: "Forge-abc1", ProxyBase: "  Preview.Example.COM. "},
			want: "https://forge-abc1.preview.example.com/",
		},
		{
			name: "proxy base: a named service takes the --<service> form",
			opts: EntryURLOptions{
				BeadID: "Forge-abc1", Service: "API_v1", ProxyBase: "preview.example.com",
			},
			want: "https://forge-abc1--api-v1.preview.example.com/",
		},
		{
			name: "proxy base: the scheme and port of the caller's own listener",
			opts: EntryURLOptions{
				BeadID: "Forge-abc1", ProxyBase: "preview.example.com",
				ProxyScheme: "http", ProxyPort: "9000",
			},
			want: "http://forge-abc1.preview.example.com:9000/",
		},
		{
			name: "proxy base: no ports allocated yet still yields a link",
			opts: EntryURLOptions{BeadID: "Forge-abc1", ProxyBase: "preview.example.com"},
			want: "https://forge-abc1.preview.example.com/",
		},
		{
			// PreviewLabel folds "" to a real label ("preview"), which would be
			// an address for a preview nobody can name.
			name: "proxy base without a bead id: no link, not a port link",
			opts: EntryURLOptions{ProxyBase: "preview.example.com", Host: "forge.wg", Port: 42002},
			want: "",
		},
		{
			name: "proxy base: the access token is appended and escaped",
			opts: EntryURLOptions{
				BeadID: "Forge-abc1", ProxyBase: "preview.example.com",
				Token: "abc+def/2==", TokenParam: "_forge_token",
			},
			want: "https://forge-abc1.preview.example.com/?_forge_token=abc%2Bdef%2F2%3D%3D",
		},
		{
			name: "a token without a parameter name is dropped",
			opts: EntryURLOptions{
				BeadID: "Forge-abc1", ProxyBase: "preview.example.com", Token: "abc",
			},
			want: "https://forge-abc1.preview.example.com/",
		},
		{
			// The token only ever authenticates a proxied request; a port link
			// reaches the service directly and there is no gate to satisfy.
			name: "a port link carries no token",
			opts: EntryURLOptions{
				BeadID: "Forge-abc1", Host: "forge.wg", Port: 42002,
				Token: "abc", TokenParam: "_forge_token",
			},
			want: "http://forge.wg:42002/",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EntryURL(tt.opts); got != tt.want {
				t.Errorf("EntryURL() = %q, want %q", got, tt.want)
			}
		})
	}
}

// The link EntryURL builds must be one ParsePreviewHost accepts, or the proxy
// would refuse the very address the dashboard handed out.
func TestEntryURL_RoundTripsThroughParsePreviewHost(t *testing.T) {
	const base = "preview.example.com"
	tests := []struct {
		beadID      string
		service     string
		wantLabel   string
		wantService string
	}{
		{beadID: "Forge-abc1", wantLabel: "forge-abc1"},
		{beadID: "Forge_ABC1", wantLabel: "forge-abc1"},
		{beadID: "Forge-abc1", service: "api", wantLabel: "forge-abc1", wantService: "api"},
		{beadID: "Forge-abc1", service: "web_ui", wantLabel: "forge-abc1", wantService: "web-ui"},
	}

	for _, tt := range tests {
		entry := EntryURL(EntryURLOptions{
			BeadID: tt.beadID, Service: tt.service, ProxyBase: base, ProxyPort: "8443",
		})
		host := entry[len("https://") : len(entry)-len("/")]
		label, service, ok := ParsePreviewHost(host, base)
		if !ok {
			t.Fatalf("ParsePreviewHost(%q) rejected a host EntryURL built", host)
		}
		if label != tt.wantLabel || service != tt.wantService {
			t.Errorf("ParsePreviewHost(%q) = (%q, %q), want (%q, %q)",
				host, label, service, tt.wantLabel, tt.wantService)
		}
	}
}

func TestServiceLabel(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"api", "api"},
		{"  API  ", "api"},
		{"api_v1", "api-v1"},
		{"api.v1", "api-v1"},
		{"", ""},
	}
	for _, tt := range tests {
		if got := ServiceLabel(tt.in); got != tt.want {
			t.Errorf("ServiceLabel(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}
