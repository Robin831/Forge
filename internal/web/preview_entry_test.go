package web

import (
	"net/http/httptest"
	"testing"

	"github.com/Robin831/Forge/internal/ipc"
	"github.com/Robin831/Forge/internal/state"
)

// TestPreviewEntryURL_WithheldForADeadEntryService: the Open link is withheld in
// both addressing forms when the entry service is not serving.
//
// The port form withholds it through previewEntryPort, but the host-based form
// is built from the bead id alone and never looks at a port — so a proxy
// deployment used to render an Open button (and mint an access token for it)
// right beside the note explaining why there was no link, which is the one
// deployment mode the note exists for.
func TestPreviewEntryURL_WithheldForADeadEntryService(t *testing.T) {
	dead := ipc.PreviewInfo{
		BeadID: "Forge-abc1",
		Services: []ipc.PreviewServiceInfo{
			{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
			{Name: "web", Port: 4311, Entry: true, Health: state.PreviewServiceExited},
		},
	}
	live := ipc.PreviewInfo{
		BeadID: "Forge-abc1",
		Services: []ipc.PreviewServiceInfo{
			{Name: "web", Port: 4311, Entry: true, Health: state.PreviewServiceHealthy},
		},
	}
	r := httptest.NewRequest("GET", "http://forge.example.com/api/previews", nil)

	tests := []struct {
		name    string
		base    string
		preview ipc.PreviewInfo
		want    string
	}{
		{"proxy base, dead entry service", "preview.example.com", dead, ""},
		// The scheme and port are the request's own: Hearth answers preview
		// hostnames on the listener the caller already reached it on.
		{"proxy base, live entry service", "preview.example.com", live, "http://forge-abc1.preview.example.com/"},
		{"no proxy base, dead entry service", "", dead, ""},
		{"no proxy base, live entry service", "", live, "http://forge.example.com:4311/"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Server{previewProxyBase: func() string { return tt.base }}
			if got := s.previewEntryURL(r, tt.preview, ""); got != tt.want {
				t.Errorf("previewEntryURL = %q, want %q", got, tt.want)
			}
		})
	}
}

// TestPreviewEntryPortMirrorsTheDaemon: the entry service is the one flagged in
// the manifest, else the first with a port — and a fallback never steps past a
// dead service to a healthy sibling, because a link that works and serves the
// wrong application is worse than no link at all.
func TestPreviewEntryPortMirrorsTheDaemon(t *testing.T) {
	tests := []struct {
		name     string
		services []ipc.PreviewServiceInfo
		wantPort int
		wantDown bool
	}{
		{name: "no services at all"},
		{
			name: "the flagged entry service wins",
			services: []ipc.PreviewServiceInfo{
				{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
				{Name: "web", Port: 4311, Entry: true, Health: state.PreviewServiceHealthy},
			},
			wantPort: 4311,
		},
		{
			name: "a single unflagged service is the entry",
			services: []ipc.PreviewServiceInfo{
				{Name: "web", Port: 4311, Health: state.PreviewServiceHealthy},
			},
			wantPort: 4311,
		},
		{
			name: "an exited entry service yields no port",
			services: []ipc.PreviewServiceInfo{
				{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
				{Name: "web", Port: 4311, Entry: true, Health: state.PreviewServiceExited},
			},
			wantDown: true,
		},
		{
			name: "an unflagged dead first service is not skipped for a healthy sibling",
			services: []ipc.PreviewServiceInfo{
				{Name: "web", Port: 4311, Health: state.PreviewServiceFailed},
				{Name: "api", Port: 4310, Health: state.PreviewServiceHealthy},
			},
			wantDown: true,
		},
		{
			name: "ports not allocated yet is not 'down'",
			services: []ipc.PreviewServiceInfo{
				{Name: "web", Entry: true, Health: state.PreviewServiceStarting},
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := previewEntryPort(tt.services); got != tt.wantPort {
				t.Errorf("previewEntryPort = %d, want %d", got, tt.wantPort)
			}
			if got := previewEntryDown(tt.services); got != tt.wantDown {
				t.Errorf("previewEntryDown = %v, want %v", got, tt.wantDown)
			}
		})
	}
}
