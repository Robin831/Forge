category: Added
- **`{{.BindHost}}` in Kiln preview manifests** - Preview manifests can now expand `{{.BindHost}}` to `preview_bind_host`, so a service that must be told what address to listen on (`vite --host`, `ASPNETCORE_URLS`) follows the setting instead of hardcoding an address that silently disagrees with it. `{{.Host}}` keeps its meaning: the public name that belongs in a URL. (Forge-hz5w)
