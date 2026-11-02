# Bead writes through Varde

With these set in the daemon's environment, every bd write the Forge makes goes to Varde (DevInfra-gbx.8); reads stay
on bd, so the Forge's database user can be read-only.

| Variable | Meaning |
| --- | --- |
| `FORGE_VARDE_URL` | Varde's base URL; the Forge calls its `/mcp` endpoint. |
| `FORGE_VARDE_KEY_FILE` | A file holding the Forge's Varde key (scopes `beads:read,beads:write,beads:file,beads:forge`). Only the path is in the environment; agent sessions inherit neither variable. |
| `FORGE_VARDE_SESSION` | The session name Varde's notes sign with. Default `Forge`. |

How it works: `executil.BdCommand` runs `forge --config <forge.yaml> bd-varde -- <bd arguments>` instead of bd for a
write, in the anvil's directory. `bd-varde` translates the arguments into Varde's bead tools and prints what bd would:

| bd write | Varde tool |
| --- | --- |
| `update --status=in_progress` | `beads_claim` |
| `update --status=open [--assignee=]` | `beads_release` (no claim to let go is not an error) |
| `update --append-notes`, `comments add` | `beads_note` |
| `update --add-label/--remove-label`, `label add/remove` | `beads_label` |
| `update --title/--description/--priority` | `beads_update` |
| `close [--force] [--reason]` | `beads_close` kind done (a bead closed already is not an error) |
| `reopen` | `beads_reopen` |
| `dep add/remove` | `beads_dep` |
| `create --deps blocks:<id>` (a split bead's part) | `beads_create` with `split_from` |
| `create --deps discovered-from:<id>` | `beads_create` under that bead's parent |
| `create` (anything else) | `beads_create` under the anvil's `beads_parent` |
| `github push` | nothing (dropped) |

Varde refuses some writes bd allowed, on purpose:
- **A dispatch label added by hand:** Hearth's tag, wicket auto-dispatch, the ledger. A hand-over goes through `beads_forge_label`, past the forge-ready gate and the owner's cap.
- **`update --notes`:** it replaces every note, and Varde's notes are append-only.
- **Setting an assignee directly:** claims go through claim and release.

The Forge itself never needs to add a dispatch label: a bead it escalates keeps its label and gets `needs-human`
(the poller skips it; a retry takes it off), and the parts of a split bead get the parent's label when they are created.
