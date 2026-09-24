category: Fixed
- **Stop no longer reopens a closed bead** - Stopping a bead from the web UI, Hearth or CLI now reopens it and clears the assignee only while the bead is still in_progress. A bead closed in the meantime (for example by the bead-closer after its PR merged) is left closed, and the skip is logged with the status found. (Fhi.Metadata-c3d0h)
