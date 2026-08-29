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

// maxNestedShellDepth bounds how far a quoted sub-command is followed. One
// level covers the form that actually occurs — `bash -c "cat x.go"`, and the
// same shape under `find -exec` — while keeping the parse finite for a command
// that nests shells all the way down.
const maxNestedShellDepth = 1

// bashShellOperators are the characters that end a token AND end a pipeline
// segment when they appear outside quotes. Each one starts a new command, so
// the token after it is a command word rather than an argument — which is
// exactly what makes `cat a.go > b.go` yield a.go and not b.go.
//
// `<` is deliberately NOT in this set (see bashInputRedirect): it is the one
// redirection whose target the command READS.
const bashShellOperators = "|;&>()`\n\r"

// bashInputRedirect ends a token without ending the segment, because what
// follows it is the file the current command reads rather than a new command
// word. Treated like the rest of the operators, `cat < internal/assay/f.go`
// made the input file a command word and dropped it — the one redirection form
// that names a file the session opened.
const bashInputRedirect = '<'

// bashEscapableChars are the characters a backslash is read as escaping. A
// backslash before anything else is kept as the literal character it is, which
// is what a Windows path pasted onto a command line consists of: `C:\w\a.go`
// would otherwise be unescaped to `C:wa.go` and match nothing, and the paths
// this parse compares against are normalized for exactly that spelling
// (normalizeSlashes). What matters is that the escapes which change the PARSE
// are honoured — a quote above all, since an unhonoured `\"` closes a string
// and desynchronises every operator after it.
const bashEscapableChars = "\"'\\ \t|;&<>()`$*?[]{}\n"

// bashPathRejectChars are the characters a token may not contain if it is to be
// read as a literal path. Globs and expansions (`*.go`, `$PKG/x.go`, `{a,b}.go`)
// name a set or a value rather than a file, and the whole point of a candidate
// is that it can be compared, byte for byte, against a path in the diff.
const bashPathRejectChars = "*?[]{}$"

// bashShellNames are the command words whose quoted argument is another command
// line rather than data.
var bashShellNames = map[string]struct{}{
	"sh": {}, "bash": {}, "zsh": {}, "dash": {}, "ksh": {},
}

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
// Getting it wrong is bounded in one direction and not the other, and it is the
// MISS that costs something. An over-match is harmless: openedDiffFiles only
// ever SELECTS from the files already in the diff, so a token naming a file
// outside the change matches nothing. A miss is not, because the retry's diff
// is scoped to what this parse resolved — a file the session read and the
// parser failed to name is a file the retry no longer sees. That is why the
// three read shapes a pass actually issues are each handled rather than left to
// the generic rules: git's `<rev>:<path>` (stripped to the path), input
// redirection (`< file`, whose target is an argument and not the next command
// word) and a quoted sub-command (`bash -c "cat x.go"`, followed one level
// down). A session whose commands resolve to nothing at all still fails open —
// an empty list means no scoping, so the retry keeps the whole diff.
func bashCandidatePaths(cmd string) []string {
	return bashCandidates(cmd, 0)
}

func bashCandidates(cmd string, depth int) []string {
	if len(cmd) > maxBashCommandBytes {
		cmd = cmd[:maxBashCommandBytes]
	}
	var out []string
	add := func(p string) bool { // reports whether there is room for more
		out = append(out, p)
		return len(out) < maxBashCandidates
	}
	for _, seg := range bashSegments(cmd) {
		nested := depth < maxNestedShellDepth && isShellCommand(segmentCommandWord(seg))
		for _, tok := range segmentArguments(seg) {
			// A shell's quoted argument is a command line, not data: the paths
			// in it were read by the command it spells out.
			if nested && strings.ContainsAny(tok, " \t") {
				for _, p := range bashCandidates(tok, depth+1) {
					if !add(p) {
						return out
					}
				}
				continue
			}
			if !pathShaped(tok) {
				continue
			}
			if !add(stripRevPrefix(tok)) {
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
//
// Backslash escapes are honoured outside single quotes — for the characters
// that change the parse (bashEscapableChars) and no others — for one reason
// above all: without them a `\"` inside a double-quoted string closed the quote
// and desynchronised every operator on the rest of the line, so segments split
// in the wrong places and arbitrary tokens were promoted to command words. An
// empty token is never emitted — an empty quoted argument (`grep` for the empty
// pattern, a `--format=` with nothing after it) would otherwise contribute a
// zero-length token that segmentArguments consumes as the command word,
// promoting the real one (`./scripts/x.sh`, which is path-shaped) to an
// argument.
func bashSegments(cmd string) [][]string {
	var (
		segments [][]string
		current  []string
		token    strings.Builder
		escaped  bool
		quote    rune // 0, '\'' or '"'
	)
	endToken := func() {
		if token.Len() > 0 {
			current = append(current, token.String())
			token.Reset()
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
		case escaped:
			if !strings.ContainsRune(bashEscapableChars, r) {
				token.WriteRune('\\') // a literal backslash, as in a Windows path
			}
			token.WriteRune(r)
			escaped = false
		case r == '\\' && quote != '\'':
			escaped = true // a backslash is literal inside single quotes only
		case quote != 0:
			if r == quote {
				quote = 0
				continue
			}
			token.WriteRune(r)
		case r == '\'' || r == '"':
			quote = r
		case r == ' ' || r == '\t':
			endToken()
		case r == bashInputRedirect:
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

// segmentCommandWord returns the token a segment invokes, skipping any leading
// environment assignments, or "" for a segment that is nothing but them.
func segmentCommandWord(tokens []string) string {
	for _, t := range tokens {
		if !envAssignment(t) {
			return t
		}
	}
	return ""
}

// isShellCommand reports whether a command word invokes a shell, whose quoted
// argument is then another command line (`bash -c "cat x.go"`). The basename is
// what is matched, so `/bin/sh` counts.
func isShellCommand(word string) bool {
	if word == "" {
		return false
	}
	w := normalizeSlashes(word)
	if i := strings.LastIndexByte(w, '/'); i >= 0 {
		w = w[i+1:]
	}
	_, ok := bashShellNames[w]
	return ok
}

// segmentArguments returns one segment's argument tokens: everything after the
// command word, minus flags and environment assignments wherever they appear.
//
// An option's own value is not tracked, so a non-dash one is kept (`grep -e
// pattern file.go` yields both `pattern` and `file.go`) and left to pathShaped
// to screen; a dash-prefixed one is dropped with the flags.
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

// stripRevPrefix reduces git's `<rev>:<path>` form to the path it names.
//
// `git show HEAD:internal/assay/skip.go` is one of the shapes a pass reads a
// file with, and the revspec is not a path anything can match: the character
// before `internal` is a colon, so pathRefersTo's suffix arms both fail and the
// file the session demonstrably read is dropped from the retry's diff. The
// prefix carries no information the comparison can use, so it is removed.
//
// A colon that follows a separator belongs to a filename rather than a revision
// and is left alone, as is a Windows drive prefix (`C:/w/a.go`), whose one
// letter would otherwise be mistaken for a revision.
func stripRevPrefix(tok string) string {
	colon := strings.IndexByte(tok, ':')
	if colon <= 0 {
		return tok
	}
	if slash := strings.IndexAny(tok, "/\\"); slash >= 0 && slash < colon {
		return tok
	}
	if colon == 1 && len(tok) > 2 && (tok[2] == '/' || tok[2] == '\\') {
		return tok // a drive letter, not a revision
	}
	rest := tok[colon+1:]
	if !pathShaped(rest) {
		return tok
	}
	return rest
}

// pathShaped reports whether a token can be read as a literal path.
//
// The bar is deliberately higher than "not a flag", because a bare word is far
// more often a subcommand, a pattern or a test name than a file: admitting
// every one of them would report `sed`'s `1,50p` and `go test`'s `TestX` as
// candidates. So a token qualifies only if it carries a path separator or a
// file extension, and only if it is literal — no glob, no expansion, no
// embedded whitespace (which can only have come from quoting, and is far more
// often a quoted command or message than a filename with a space in it).
//
// It is deliberately looser than fileShaped, which is what the `files=`
// telemetry count is taken over: a candidate that is not a file — a directory,
// a fragment of a quoted script — simply matches nothing in the diff, whereas
// the same token counted as a file read is a false claim about what the pass
// went and looked at.
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

// fileShaped reports whether a tracked path names a FILE, which is the question
// the `files=` telemetry field answers and a stricter one than pathShaped's.
//
// The counter exists so that a non-zero value is evidence a pass went and read
// code, which makes every non-file it counts a false positive in exactly the
// signal it was added for: `cd internal/assay && go test ./...` opens nothing
// and reported `files=1`, and a quoted script fragment
// (`print(open('x.go').read())`) reported another. So the count is taken over
// tokens whose last component carries a file extension.
//
// It undercounts an extensionless file (`Makefile`, `scripts/run`), which is
// the safe direction for a figure whose whole value is that a non-zero reading
// can be believed.
func fileShaped(p string) bool {
	s := normalizeSlashes(p)
	if i := strings.LastIndexAny(s, "/\\"); i >= 0 {
		s = s[i+1:]
	}
	return hasFileExtension(s)
}

// countFilesRead is how many of the paths a session named are files it read.
// It is the fold behind PassReport.FilesRead, applied wherever a tracked list
// becomes a count, so the list itself stays the looser one the retry's diff
// scoping selects from.
func countFilesRead(paths []string) int {
	n := 0
	for _, p := range paths {
		if fileShaped(p) {
			n++
		}
	}
	return n
}

// maxFileExtensionBytes bounds the run of characters after the final dot that
// can still be read as an extension. It is what decides that `foo.markdown` (8)
// is path-shaped while `foo.something` (9) is not: real extensions are short,
// and the long tail past this bound is dominated by version strings and
// sentence fragments that happen to contain a dot.
const maxFileExtensionBytes = 8

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
	if len(ext) > maxFileExtensionBytes {
		return false
	}
	for _, r := range ext {
		if !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9') {
			return false
		}
	}
	return true
}
