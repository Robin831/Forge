category: Added
- **Add-comment composer on the bead detail page** - The Comments panel now includes a textarea + "Add comment" button that POSTs to a new `POST /api/bead/{id}/comment` endpoint, shelling out to `bd comments add` and optimistically appending the created comment to the list. (Forge-o3pr)
