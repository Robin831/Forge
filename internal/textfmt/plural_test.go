package textfmt

import "testing"

func TestCount(t *testing.T) {
	tests := []struct {
		n    int
		noun string
		want string
	}{
		{0, "quest", "0 quests"},
		{1, "quest", "1 quest"},
		{2, "quest", "2 quests"},
		{1, "dependent", "1 dependent"},
		{3, "restart", "3 restarts"},
	}
	for _, tt := range tests {
		if got := Count(tt.n, tt.noun); got != tt.want {
			t.Errorf("Count(%d, %q) = %q, want %q", tt.n, tt.noun, got, tt.want)
		}
	}
}

// Suffix carries the counts the callers actually reach it with: zero is plural
// ("0 beads"), which is the case a naive `n > 1` would get wrong.
func TestSuffix(t *testing.T) {
	for n, want := range map[int]string{0: "s", 1: "", 2: "s", 17: "s"} {
		if got := Suffix(n); got != want {
			t.Errorf("Suffix(%d) = %q, want %q", n, got, want)
		}
	}
}
