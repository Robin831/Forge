category: Added
- **Pause/resume a bead from the Web UI** - New `POST /api/bead/{id}/pause` and `/resume` endpoints proxy the pause_bead/resume_bead IPC verbs, with pause/resume buttons in the Workers pane and worker log modal (pause enabled while running, resume enabled while paused) and actor-audited `bead_paused`/`bead_resumed` events surfaced in the live activity feed. (Forge-7h49)
