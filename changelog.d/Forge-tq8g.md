category: Added
- **Per-stage provider configuration** - New `stage_providers` config map allows separate model/provider chains for each pipeline stage (smith, warden, schematic, cifix, reviewfix), enabling cost optimization by using cheaper models for simpler stages. Falls back to `smith_providers` then `providers` when a stage key is not set. (Forge-tq8g)
