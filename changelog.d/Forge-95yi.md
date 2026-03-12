category: Added
- **Warden validates diff against bead description to catch scope drift** - The Warden review prompt now includes the bead title and description, enabling a 6th check: whether the diff actually implements what the bead requested. This catches partial implementations, scope drift, and cases where the Smith went off on a tangent. (Forge-95yi)
