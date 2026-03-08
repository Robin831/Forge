category: Added
- **Auto-learn warden rules via PR after merge** - When `auto_learn_rules` is enabled, bellows now creates a reviewable PR with learned warden rules from Copilot comments on merged PRs, instead of writing directly to the anvil. This keeps rule changes visible and reviewable. A new `warden_rule_learned` event is logged when the rules PR is created. (Forge-kfu)
