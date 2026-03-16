category: Fixed
- **Prevent smith from misusing NO_CHANGES_NEEDED in warden review iterations** - `NO_CHANGES_NEEDED` is now hidden from the smith prompt on warden review iterations (iteration 2+). If smith emits it anyway or produces no diff during a review iteration, the pipeline escalates to `needs_human` instead of looping indefinitely. (Forge-6gas)
