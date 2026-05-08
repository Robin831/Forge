package forgechat

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// ModeEmit is the per-turn intent that asks claude to translate the settled
// design into a structured list of beads to create. It runs in the "ready"
// stage (post-grilling) and is the only mode that produces side-effects in
// bd databases.
const ModeEmit Mode = "emit"

// validBeadTypes mirrors the issue type set documented in AGENTS.md. We use a
// closed set so we can refuse silly types up front rather than discovering
// them when bd rejects the create.
var validBeadTypes = map[string]bool{
	"bug":      true,
	"feature":  true,
	"task":     true,
	"chore":    true,
	"epic":     true,
	"decision": true,
}

// BeadProposal is one bead claude wants to create. The ProposalID is a stable
// identifier within a single emission so DependsOn can reference siblings
// before the real bead IDs exist (we only learn those after bd create).
type BeadProposal struct {
	ProposalID  string   `json:"id"`
	Anvil       string   `json:"anvil"`
	Title       string   `json:"title"`
	Description string   `json:"description"`
	Type        string   `json:"type"`
	Priority    int      `json:"priority"`
	Labels      []string `json:"labels,omitempty"`
	// DependsOn is a list of sibling ProposalIDs that must complete before
	// this bead. Cross-anvil deps are rejected up front because bd's
	// dep-store is anvil-local — a "depends on" link from anvil X to anvil Y
	// would never resolve, so we refuse rather than silently break.
	DependsOn []string `json:"depends_on,omitempty"`
}

// EmissionEnvelope is the JSON shape claude returns for ModeEmit. Summary is
// optional and shown to the user verbatim above the proposal list.
type EmissionEnvelope struct {
	Beads   []BeadProposal `json:"beads"`
	Summary string         `json:"summary,omitempty"`
}

// ParseEmissionResponse extracts the JSON envelope from claude output. It
// tolerates fenced ```json blocks, plain ``` blocks, and bare JSON.
func ParseEmissionResponse(output string) (*EmissionEnvelope, error) {
	if env, err := tryParseEmissionFenced(output); err == nil {
		return env, nil
	}
	// Fallback: find the first JSON object that decodes as an EmissionEnvelope.
	// json.Decoder handles braces and fences inside string values correctly,
	// unlike a hand-rolled brace counter.
	for i := 0; i < len(output); i++ {
		if output[i] != '{' {
			continue
		}
		var env EmissionEnvelope
		dec := json.NewDecoder(strings.NewReader(output[i:]))
		if err := dec.Decode(&env); err == nil && len(env.Beads) > 0 {
			return &env, nil
		}
	}
	return nil, errors.New("no valid emission envelope JSON found in output")
}

// findClosingFence returns the index within s where a line-starting closing
// fence (```) begins — i.e. "```" immediately after a '\n'. Returns -1 if
// no such fence is found. Requiring the fence to start at column 0 prevents
// "```" appearing inside a JSON string value (which is encoded on a single
// line in the output) from being mistaken for the end of the fenced block.
func findClosingFence(s string) int {
	search := s
	offset := 0
	for {
		idx := strings.Index(search, "\n```")
		if idx < 0 {
			return -1
		}
		after := search[idx+4:]
		if len(after) == 0 || after[0] == '\n' || after[0] == '\r' {
			return offset + idx + 1
		}
		search = search[idx+1:]
		offset += idx + 1
	}
}

func tryParseEmissionFenced(output string) (*EmissionEnvelope, error) {
	if idx := strings.Index(output, "```json"); idx >= 0 {
		start := idx + len("```json")
		if end := findClosingFence(output[start:]); end >= 0 {
			var env EmissionEnvelope
			if err := json.Unmarshal([]byte(strings.TrimSpace(output[start:start+end])), &env); err == nil {
				return &env, nil
			}
		}
	}
	if idx := strings.Index(output, "```"); idx >= 0 {
		start := idx + 3
		if nl := strings.Index(output[start:], "\n"); nl >= 0 {
			start += nl + 1
		}
		if end := findClosingFence(output[start:]); end >= 0 {
			var env EmissionEnvelope
			if err := json.Unmarshal([]byte(strings.TrimSpace(output[start:start+end])), &env); err == nil {
				return &env, nil
			}
		}
	}
	return nil, fmt.Errorf("no fenced JSON block")
}

// ValidateEmission checks the envelope is internally consistent and can be
// materialised. Returns a list of human-readable problems; an empty list
// means the envelope passed every check. knownAnvils maps anvil name -> true
// for every anvil registered with the daemon; pass nil to skip the anvil
// existence check (useful in tests where the resolver is not available).
//
// Checks:
//   - at least one bead present
//   - every bead has non-empty title and proposal id
//   - proposal ids are unique within the envelope
//   - type is one of the known bd types
//   - priority is in [0, 4]
//   - each anvil exists in knownAnvils (when supplied)
//   - depends_on entries reference real sibling proposal ids
//   - depends_on never crosses anvils (bd cannot link cross-anvil)
//   - the dep graph is a DAG (no cycles)
func ValidateEmission(env *EmissionEnvelope, knownAnvils map[string]bool) []string {
	var problems []string
	if env == nil || len(env.Beads) == 0 {
		return []string{"emission contains no beads"}
	}

	// Build a case-folded map so anvil lookups match the daemon's
	// case-insensitive resolveAnvilConfig behaviour (e.g. "Munin" matches the
	// configured key "munin"). We also rewrite env.Beads[i].Anvil to the
	// canonical key so downstream routing uses the correct casing.
	var canonicalAnvil map[string]string
	if knownAnvils != nil {
		canonicalAnvil = make(map[string]string, len(knownAnvils))
		for name := range knownAnvils {
			if knownAnvils[name] {
				canonicalAnvil[strings.ToLower(name)] = name
			}
		}
	}

	idIndex := make(map[string]int, len(env.Beads))
	for i, b := range env.Beads {
		id := strings.TrimSpace(b.ProposalID)
		if id == "" {
			problems = append(problems, fmt.Sprintf("bead #%d: missing proposal id", i+1))
			continue
		}
		if _, dup := idIndex[id]; dup {
			problems = append(problems, fmt.Sprintf("bead #%d: duplicate proposal id %q", i+1, id))
			continue
		}
		idIndex[id] = i
	}

	for i, b := range env.Beads {
		if strings.TrimSpace(b.Title) == "" {
			problems = append(problems, fmt.Sprintf("bead %q: missing title", b.ProposalID))
		}
		if strings.TrimSpace(b.Anvil) == "" {
			problems = append(problems, fmt.Sprintf("bead %q: missing anvil", b.ProposalID))
		} else if canonicalAnvil != nil {
			if canonical, ok := canonicalAnvil[strings.ToLower(b.Anvil)]; ok {
				env.Beads[i].Anvil = canonical
			} else {
				problems = append(problems, fmt.Sprintf("bead %q: anvil %q is not registered with this forge", b.ProposalID, b.Anvil))
			}
		}
		typ := strings.ToLower(strings.TrimSpace(b.Type))
		if typ == "" {
			env.Beads[i].Type = "task"
		} else if !validBeadTypes[typ] {
			problems = append(problems, fmt.Sprintf("bead %q: type %q is not a valid bd issue type", b.ProposalID, b.Type))
		} else {
			env.Beads[i].Type = typ
		}
		if b.Priority < 0 || b.Priority > 4 {
			problems = append(problems, fmt.Sprintf("bead %q: priority %d outside [0,4]", b.ProposalID, b.Priority))
		}
		for _, dep := range b.DependsOn {
			depID := strings.TrimSpace(dep)
			if depID == "" {
				problems = append(problems, fmt.Sprintf("bead %q: empty depends_on entry", b.ProposalID))
				continue
			}
			if depID == b.ProposalID {
				problems = append(problems, fmt.Sprintf("bead %q: cannot depend on itself", b.ProposalID))
				continue
			}
			depIdx, ok := idIndex[depID]
			if !ok {
				problems = append(problems, fmt.Sprintf("bead %q: depends on unknown proposal %q", b.ProposalID, depID))
				continue
			}
			if env.Beads[depIdx].Anvil != "" && b.Anvil != "" && env.Beads[depIdx].Anvil != b.Anvil {
				problems = append(problems, fmt.Sprintf(
					"bead %q (anvil %q) depends on %q (anvil %q): cross-anvil dependencies are not supported",
					b.ProposalID, b.Anvil, depID, env.Beads[depIdx].Anvil,
				))
			}
		}
	}

	if cycle := detectCycle(env.Beads); cycle != "" {
		problems = append(problems, "depends_on graph contains a cycle: "+cycle)
	}

	sort.Strings(problems)
	return problems
}

// detectCycle returns a human-readable description of any cycle in the
// depends_on graph, or "" when the graph is a DAG. Uses iterative DFS so a
// pathological deeply-nested graph cannot blow the stack.
func detectCycle(beads []BeadProposal) string {
	adj := make(map[string][]string, len(beads))
	for _, b := range beads {
		adj[b.ProposalID] = b.DependsOn
	}
	const (
		unvisited = 0
		onStack   = 1
		done      = 2
	)
	color := make(map[string]int, len(beads))
	for _, b := range beads {
		if color[b.ProposalID] != unvisited {
			continue
		}
		// Iterative DFS with an explicit stack so we can record the path
		// that produces the cycle for diagnostics.
		type frame struct {
			node string
			next int
		}
		stack := []frame{{node: b.ProposalID}}
		path := []string{b.ProposalID}
		color[b.ProposalID] = onStack
		for len(stack) > 0 {
			top := &stack[len(stack)-1]
			children := adj[top.node]
			if top.next >= len(children) {
				color[top.node] = done
				stack = stack[:len(stack)-1]
				if len(path) > 0 {
					path = path[:len(path)-1]
				}
				continue
			}
			child := children[top.next]
			top.next++
			switch color[child] {
			case unvisited:
				color[child] = onStack
				path = append(path, child)
				stack = append(stack, frame{node: child})
			case onStack:
				// Build the cycle slice from path[indexOf(child):...] + child.
				start := 0
				for i, p := range path {
					if p == child {
						start = i
						break
					}
				}
				cycle := append([]string{}, path[start:]...)
				cycle = append(cycle, child)
				return strings.Join(cycle, " -> ")
			}
		}
	}
	return ""
}

// systemPromptEmit is the system prompt for ModeEmit. It tells claude to
// translate the settled plan + grilling answers into a flat list of beads
// with explicit anvil routing and sibling dependency edges.
const systemPromptEmit = `You are translating a finalised Beads-Forge design into a list of bd issues to create.

Input you have:
- The plan agreed during drafting (above).
- The full conversation, including grilling Q&A.
- The set of registered anvils (below) — beads MUST target one of these by name.

Your job: emit a JSON envelope listing every bead the user wants created. One bead per cohesive unit of work. Prefer multiple small beads with explicit dependencies over one mega-bead. Each bead must be self-contained enough that an AI agent can implement it without needing to read the others.

Hard rules:
- Every bead targets exactly one anvil (the bd database lives in that anvil; cross-anvil deps are NOT supported).
- depends_on lists sibling proposal ids ONLY (the strings in the "id" field). Do not invent bd-* ids.
- A bead in anvil X may not depend on a bead in anvil Y. Split the work along anvil lines if needed.
- The depends_on graph must be a DAG.
- Use bd issue types: bug | feature | task | chore | epic | decision.
- Priority is 0 (highest) to 4 (lowest). Default 2 unless something is clearly more or less urgent.

Do NOT use any tools. Do NOT modify the filesystem. Do NOT explore the codebase. Reply with ONLY the JSON envelope (no preamble, no commentary).`

const tailEmit = "Output the JSON envelope NOW as a fenced ```json ... ``` block.\n\n" +
	"Shape:\n\n" +
	"```json\n" +
	"{\n" +
	"  \"summary\": \"One sentence describing the proposed split (optional).\",\n" +
	"  \"beads\": [\n" +
	"    {\n" +
	"      \"id\": \"p1\",\n" +
	"      \"anvil\": \"<one of the registered anvil names>\",\n" +
	"      \"title\": \"Concise imperative title\",\n" +
	"      \"description\": \"Markdown description with enough detail for an AI agent to implement.\",\n" +
	"      \"type\": \"feature\",\n" +
	"      \"priority\": 2,\n" +
	"      \"labels\": [\"area:auth\"],\n" +
	"      \"depends_on\": []\n" +
	"    },\n" +
	"    {\n" +
	"      \"id\": \"p2\",\n" +
	"      \"anvil\": \"<same or different registered anvil>\",\n" +
	"      \"title\": \"Follow-up that depends on p1\",\n" +
	"      \"description\": \"...\",\n" +
	"      \"type\": \"task\",\n" +
	"      \"priority\": 2,\n" +
	"      \"depends_on\": [\"p1\"]\n" +
	"    }\n" +
	"  ]\n" +
	"}\n" +
	"```\n\n" +
	"Use \"id\" values that are short and unique within this envelope (e.g. p1, p2, p3). They are NEVER persisted as bd ids — they exist only to express depends_on edges before the real bd ids are assigned."

// AnvilContext is rendered into the emission prompt so claude knows which
// anvil names are valid. Keys are anvil names; values are short hints
// (typically the path or a short description).
type AnvilContext map[string]string

// formatAnvilContext renders the anvil list into a markdown bullet section
// for the prompt. Empty map produces an empty string.
func formatAnvilContext(anvils AnvilContext) string {
	if len(anvils) == 0 {
		return ""
	}
	keys := make([]string, 0, len(anvils))
	for k := range anvils {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString("\n\n## Registered anvils\n\n")
	for _, k := range keys {
		hint := anvils[k]
		if hint != "" {
			fmt.Fprintf(&b, "- `%s` — %s\n", k, hint)
		} else {
			fmt.Fprintf(&b, "- `%s`\n", k)
		}
	}
	return b.String()
}
