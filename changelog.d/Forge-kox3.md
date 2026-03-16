fix: run bead close in background after IPC merge to avoid timeout (Forge-kox3)

The bead close added in Forge-xxn4 ran synchronously inside the merge_pr
IPC handler before the response was sent back. The IPC client Send() has
a 10-second read deadline; if the GitHub merge + bd close together took
longer than that, Hearth received a timeout error and showed "Failed to
merge PR #N" in the status bar even though both the merge and the bead
close had actually succeeded.

The bead close (and the subsequent bellows Refresh) are now run in a
goroutine so the ok response is returned to Hearth immediately after the
merge succeeds.
