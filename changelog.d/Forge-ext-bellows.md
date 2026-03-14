category: Fixed
- Pre-existing external PRs (`ext-*`) no longer incorrectly receive bellows lifecycle management (cifix, reviewfix, rebase) after upgrade — a data fixup now resets `bellows_managed=0` for all `ext-*` PRs on startup
