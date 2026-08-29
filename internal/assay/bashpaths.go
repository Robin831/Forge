package assay

import "strings"

// maxBashCommandBytes bounds how much of one command string is parsed for
// paths. The string is model output, so it is arbitrary: a heredoc, a base64
// blob or a generated script can be megabytes, and every byte past the first
// few kilobytes is a byte no reviewer typed a path into. Truncation is safe in
// the direction that matters — a token cut in half stops matching a diff path,
// so an over-long command loses candidates rather than gaining wrong ones.
const maxBashCommandBytes = 16384

// maxBashCandidates bounds how many candidates one command yields. A command
// naming forty paths is a bulk operation, not a reviewer reading code, and the
// tracker's own maxTrackedFiles cap is a whole-session bound rather than a
// per-call one.
const maxBashCandidates = 32

// bashShellOperators are the characters that end a token AND end a pipeline
// segment when they appear outside quotes. Each one starts a new command, so
// the token after it is a command word rather than an argument — which is
// exactly what makes `cat a.go > b.go` yield a.go and not b.go.
const bashShellOperators = "|;&<>()`\n\r"

// bashPathRejectChars are the characters a token may not contain if it is to be
// read as a literal path. Globs and expansions (`*.go`, `$PKG/x.go`, `{a,b}.go`)
// name a set or a value rather than a file, and the whole point of a candidate
// is that it can be compared, byte for byte, against a path in the diff.
const bashPathRejectChars = "*?[]{}$"

// bashCandidatePaths extracts the path-shaped arguments of a shell command.
//
// It exists because the structured half of fileTracker cannot see what an Assay
// pass actually does: measured over 95 post-reading-prompt pass sessions, all
// 742 tool calls were Bash and not one carried a `file_path` input, so both
// things that read the tracked file list — the `files=` telemetry field and the
// retry's third modification, the diff scoped to what the failed session opened
// — were structurally zero. A pass that reads ten files with `cat`, `sed` and
// `grep` looked identical to one that read none.
//
// The parse is deliberately conservative and deliberately not a shell. It
// tokenises on whitespace and on the operators that separate one command from
// the next, strips one layer of quoting, drops flags, environment assignments
// and each segment's command word, and keeps what is left only if it still
// looks like a literal path. Everything it cannot resolve — a variable, a glob,
// a subshell — is dropped rather than guessed at, because a candidate that
// names nothing is a candidate that will match nothing.
//
// Getting it wrong is bounded on the side that matters: openedDiffFiles only
// ever SELECTS from the files already in the diff, so a misparsed token can
// narrow a retry's scope by less than it should have, never widen it past the
// change under review.
func bashCandidatePaths(cmd string) []string {
	if len(cmd) > maxBashCommandBytes {
		cmd = cmd[:maxBashCommandBytes]
	}
	var out []string
	for _, seg := range bashSegments(cmd) {
		for _, tok := range segmentArguments(seg) {
			if !pathShaped(tok) {
				continue
			}
			out = append(out, tok)
			if len(out) >= maxBashCandidates {
				return out
			}
		}
	}
	return out
}

// bashSegments splits a command line into one token slice per pipeline segment.
//
// Quoting is tracked so an operator or a space inside quotes does not split a
// token, and one layer of quotes is removed as the token is closed — the value
// wanted is the path the shell would pass, not the text that spells it.
func bashSegments(cmd string) [][]string {
	var (
		segments [][]string
		current  []string
		token    strings.Builder
		quoted   bool // this token carried quotes, so its spaces are literal
		quote    rune // 0, '\'' or '"'
	)
	endToken := func() {
		if token.Len() > 0 || quoted {
			current = append(current, token.String())
			token.Reset()
			quoted = false
		}
	}
	endSegment := func() {
		endToken()
		if len(current) > 0 {
			segments = append(segments, current)
			current = nil
		}
	}
	for _, r := range cmd {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			token.WriteRune(r)
		case r == '\'' || r == '"':
			quote, quoted = r, true
		case r == ' ' || r == '\t':
			endToken()
		case strings.ContainsRune(bashShellOperators, r):
			endSegment()
		default:
			token.WriteRune(r)
		}
	}
	endSegment()
	return segments
}

// segmentArguments returns one segment's argument tokens: everything after the
// command word, minus flags and anything shaped like an option's own value that
// begins with a dash.
//
// Leading environment assignments (`LC_ALL=C git show ...`) precede the command
// word rather than being it, so they are skipped without consuming the drop.
func segmentArguments(tokens []string) []string {
	i := 0
	for i < len(tokens) && envAssignment(tokens[i]) {
		i++
	}
	if i < len(tokens) {
		i++ // the command word itself
	}
	var args []string
	for _, t := range tokens[i:] {
		if strings.HasPrefix(t, "-") || envAssignment(t) {
			continue
		}
		args = append(args, t)
	}
	return args
}

// envAssignment reports whether a token is a `NAME=value` assignment rather
// than a path. The `=` has to come before any `/`, since a path may legitimately
// contain one further down (`a/b=c.txt`) while an assignment's name may not
// contain a separator.
func envAssignment(tok string) bool {
	eq := strings.IndexByte(tok, '=')
	if eq <= 0 {
		return false
	}
	slash := strings.IndexByte(tok, '/')
	return slash < 0 || eq < slash
}

// pathShaped reports whether a token can be read as a literal path.
//
// The bar is deliberately higher than "not a flag", because this token also
// feeds the `files=` telemetry field, which is a claim about how many files a
// pass read: admitting every bare word would report `sed`'s `1,50p` and `go
// test`'s `TestX` as files and make the counter as uninformative as the zero it
// replaces. So a token qualifies only if it carries a path separator or a file
// extension, and only if it is literal — no glob, no expansion, no embedded
// whitespace (which can only have come from quoting, and is far more often a
// quoted command or message than a filename with a space in it).
func pathShaped(tok string) bool {
	if tok == "" || strings.ContainsAny(tok, bashPathRejectChars) {
		return false
	}
	if strings.ContainsAny(tok, " \t") {
		return false
	}
	if strings.Trim(tok, "./\\") == "" {
		return false // ".", "..", "./...", "/" — a path expression naming nothing
	}
	return strings.ContainsAny(tok, "/\\") || hasFileExtension(tok)
}

// hasFileExtension reports whether a token ends in a plausible file extension:
// a dot followed by a short run of alphanumerics, with something before it. It
// is what lets a bare `retry.go` through while keeping `1,50p` and `v1.2.3-rc`
// out.
func hasFileExtension(tok string) bool {
	dot := strings.LastIndexByte(tok, '.')
	if dot <= 0 || dot == len(tok)-1 {
		return false
	}
	ext := tok[dot+1:]
	if len(ext) > 8 {
		return false
	}
	for _, r := range ext {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
