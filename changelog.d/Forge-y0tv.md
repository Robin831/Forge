category: Added
- **Bellows mute from the dashboard** - `POST /api/prs/{id}/bellows/detach` and `/bellows/resume` dispatch the `detach_bellows` / `reattach_bellows` actions, the browser half of `forge bellows stop|resume`. Both work for forge-authored and `ext-*` PRs. (Forge-y0tv)
- **`bellows_detached` on the PR payload** - The `/api/prs/all` rows now carry the mute flag, so a muted PR can be rendered as detached instead of as `monitoring`. (Forge-y0tv)
