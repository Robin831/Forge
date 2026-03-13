category: Fixed

- Fix Workers panel table corruption caused by ANSI escape sequences in cell values — Bubbles table internally calls runewidth.Truncate which does not handle ANSI, breaking row alignment (Forge-naby)
- Change Workers panel selected row background from bright orange to subtle gray for readability (Forge-naby)
