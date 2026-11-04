# Bead writes through Varde

Without the two variables below nothing here applies: the Forge writes with bd exactly as 0.37.3 did, and its smiths
get 0.37.2's beads environment and instructions. With them set in the daemon's environment, every bd write the Forge makes goes to Varde (DevInfra-gbx.8), and so does
every read (DevInfra-rjb.6, below): the Forge runs no bd of its own.

| Variable | Meaning |
| --- | --- |
| `FORGE_VARDE_URL` | Varde's base URL; the Forge calls its `/mcp` endpoint. |
| `FORGE_VARDE_KEY_FILE` | A file holding the Forge's Varde key (scopes `beads:read,beads:write,beads:file,beads:forge`). Only the path is in the environment; agent sessions inherit neither variable. |
| `FORGE_VARDE_SESSION` | The session name Varde's notes sign with. Default `Forge`. |
| `FORGE_VARDE_SMITH_KEY_FILE` | Optional. A file holding the smiths' own key (scopes `beads:read,beads:file`: create under the owner's open epic, note, add a plain label). With it, the session that implements a bead files discovered issues itself (below). |

How it works: `executil.BdCommand` runs `forge --config <forge.yaml> bd-varde -- <bd arguments>` instead of bd for a
write, in the anvil's directory. `bd-varde` translates the arguments into Varde's bead tools and prints what bd would:

| bd write | Varde tool |
| --- | --- |
| `update --status=in_progress` | `beads_claim` |
| `update --status=open [--assignee=]` | `beads_release` (no claim to let go is not an error); label changes in the same call go first |
| `update --append-notes`, `comments add` | `beads_note` |
| `update --add-label/--remove-label`, `label add/remove` | `beads_label` |
| `update --title/--description/--priority` | `beads_update` |
| `close [--force] [--reason]` | `beads_close`: kind `not-doing` for a rollback, a discard or a stop, else `done` (a bead closed already is not an error) |
| `reopen` | `beads_reopen` |
| `dep add/remove` | `beads_dep` |
| `create --deps blocks:<id>` (a split bead's part) | `beads_create` with `split_from` |
| `create --deps discovered-from:<id>` | `beads_create` under that bead's parent |
| `create --deps <id>` (depends-on) | `beads_create` under the anvil's `beads_parent`, then `beads_dep` |
| `create` (anything else) | `beads_create` under the anvil's `beads_parent` |
| `github push` | nothing (dropped); the sub-bead issue sync and the external-ref lookup are skipped |

A flag with no counterpart in Varde is refused, so nothing is dropped silently. Text over Varde's limits is cut with a
marker: title 200, description 8000, note 4000, reason 500 characters. A 429 or 5xx from Varde is retried twice
(2 s, 6 s); a refusal is not.

**Use `auto_dispatch: tagged` on every anvil.** The Forge's key writes only beads carrying its own Forge label, so a
bead dispatched without one (`auto_dispatch: all` or `priority`) cannot be claimed; the daemon warns at start.

Varde refuses some writes bd allowed, on purpose:
- **A dispatch label added by hand:** Hearth's tag, wicket auto-dispatch, the ledger. A hand-over goes through `beads_forge_label`, past the forge-ready gate and the owner's cap.
- **`update --notes`:** it replaces every note, and Varde's notes are append-only.
- **Setting an assignee directly:** claims go through claim and release.
- **The ledger's kanban moves to blocked, deferred or closed by status:** they are not claim or release.

The Forge itself never needs to add a dispatch label: a bead it escalates keeps its label and gets `needs-human`
(the poller skips it; a retry takes it off), and the parts of a split bead get the parent's label when they are created.

## Smiths filing what they find

With `FORGE_VARDE_SMITH_KEY_FILE` set, the implementing smith (and its steer resumes) runs with
`--mcp-config ~/.forge/varde/smith-mcp.json` (mode 0600, outside every worktree), which names Varde as the
`varde-smith` server with the smiths' key. Its prompt tells it to file each discovered issue with `beads_create` under the anvil's `beads_parent`. Varde's
`beads:file` scope also lets the key note a bead and add a label (never a forge, `venter:` or owner-only one), but
never claim, close, reopen, retitle, remove a label, set a dependency or make an epic; it files only under the
owner's own open epic. A narrower key, not a boundary. Only
Claude reads `--mcp-config`. Without the key or without `beads_parent` the prompt asks for a «Discovered issues» list
in the smith's final message; with another provider (a fallback after Claude) the prompt still names the tool, and the
smith lists what it finds because the tool is not there, as the prompt tells it to. Review, triage and the other stages never get the tools.

The key file is readable to the smith's uid like the Forge's own; smiths under their own uid is DevInfra-rjb.1.

## Bead reads through Varde

In the same mode `executil.BdCommand` runs every bd command that is not a write as
`forge --config <forge.yaml> bd-varde --base <dir> --read -- <bd arguments>`, in the caller's directory (the anvil's).
`bd-varde --read` finds the anvil from that directory, the anvil's Varde pool, and calls Varde's `beads_bd_read`
(`{pool, args}`; the Forge's key, scopes `beads:read` and `beads:forge`). Varde runs `bd --readonly --sandbox <args>`
live in the pool's workspace, so the answer is as fresh as bd's own. `bd-varde` prints bd's stdout and stderr as Varde
passed them and exits with bd's exit code, so every parser of bd's output works unchanged, bd's not-found JSON on exit 1
included.

The reads Varde runs, and so the only ones the Forge sends: `ready`, `list`, `show` (positional ids or `--id=`, up to
200, with `--include-dependents`) and `comments <id>`, with `--json`, `--limit`, `--label`, `--exclude-label` (ready),
`--status` (list) and `--id` (show). A list of closed beads needs a `--limit` above 0. `vardebeads.IsRead` mirrors
Varde's whitelist (`varde/app/bd-read.mjs`); anything else is refused by `bd-varde` before it reaches Varde, and is never
handed to a local bd.

- **Refusals:** `Refused (503)` (Varde's read queue full, or a wait over 10 s) and `Refused (504)` (bd over 30 s) are
  retried after 1 s, 3 s and 6 s, within the caller's bd timeout. Any other refusal (400: not a read Varde runs, or an
  id from another pool; 404: no such pool; 502: bd failed or the answer is over 4 MB) is one stderr line and exit 1.
- **The pool:** an anvil's `varde_pool`, else read from its git-tracked `.beads` files, following `.beads/redirect`:
  `issue-prefix` in `config.yaml`, the id prefix of `issues.jsonl`, then the database named at the end of
  `sync.remote`, each matched against Varde's pools (`munin`: `Fhi.Metadata`, `heimdall`: `Heimdall`, `devinfra`:
  `DevInfra`). With none of them naming one, the read fails and says to set `varde_pool`.
- **What changes in the Forge:** the anvil health check (Dolt `sql` for merge conflicts) is skipped, logged once: there
  is no local clone to wedge, and Varde stops its own writes on a dirty pool. depcheck and the ledger go straight to their
  `bd list` fallbacks instead of `bd sql`. `forge doctor` checks each anvil's reads through Varde
  (`beads_bd_read list --limit 1`) instead of bd on PATH, its dependents flag and the database.
- **Varde down:** reads fail as writes do, so the poller finds no work and nothing is dispatched until Varde answers.
  There is no fallback to bd.
