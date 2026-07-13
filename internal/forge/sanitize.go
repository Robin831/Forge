package forge

import "strings"

// SanitizeBeadID makes a bead ID safe to embed in a file path by replacing
// path separators and stripping ".." segments to prevent directory traversal.
func SanitizeBeadID(id string) string {
	s := strings.ReplaceAll(id, "/", "_")
	s = strings.ReplaceAll(s, `\`, "_")
	s = strings.ReplaceAll(s, "..", "__")
	return s
}
