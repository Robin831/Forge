## Forge-6gas: Prevent smith from misusing NO_CHANGES_NEEDED in warden review iterations

`NO_CHANGES_NEEDED` is semantically "the original bead work was already done
before I started". It is not a valid response to warden review feedback.

Two fixes:

1. The smith prompt now hides the `NO_CHANGES_NEEDED` section on iteration 2+
   (when `PriorFeedback` is set), so the signal is not available during review
   iterations.

2. If smith emits `NO_CHANGES_NEEDED` in a review iteration anyway (e.g. via a
   custom repo CLAUDE.md), the pipeline now treats it as `needs_human` instead
   of closing the bead.

3. If smith makes no code changes at all in a review iteration (empty git diff),
   the pipeline escalates to `needs_human` — continuing the loop would just have
   warden reject with the same feedback again.
