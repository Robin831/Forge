category: Fixed
- **AnvilSettings TypeScript type covers the Kiln preview keys** - The frontend `AnvilSettings` interface was missing `preview_enabled`, `preview_auto` and `preview_quests`, which `config.AnvilSettings` has always serialized, so reading them from `anvils[name]` was a type error even though the fields were present in the JSON. (Forge-mu5i)
