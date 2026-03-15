## Forge-74a0: Prevent re-dispatch of decomposed parent beads

When a decomposed parent bead has dependents it must stay open (so those
dependents remain blocked). Once all dependents complete the parent becomes
unblocked and is re-dispatched — but there is no work left to do.

The daemon now tags the parent with `forge-decomposed` when keeping it open.
On re-dispatch, schematic detects the label, returns `ActionAlreadyDecomposed`,
and the pipeline closes the bead via the existing no-changes-needed path
without spawning a smith or running any AI session.
