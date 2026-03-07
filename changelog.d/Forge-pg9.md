category: Fixed
- **Fixed FetchCopilotComments failing on PRs with >30 review comments** - `gh api --paginate` concatenates multiple JSON arrays which `json.Unmarshal` cannot parse. Switched to a streaming `json.Decoder` to handle concatenated array output correctly. (Forge-pg9)
