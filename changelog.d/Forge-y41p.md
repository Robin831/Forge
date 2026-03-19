category: Added
- **Ingot lifecycle tracking in pipeline** - Every bead processed through pipeline.Run() now gets a corresponding ingot record tracking its journey through init, smith, temper, warden, approved, and failed stages. Temper step results are recorded as structured test results. All ingot writes are best-effort and never fail the pipeline. (Forge-y41p)
