category: Fixed
- **reviewfix and cifix now run tests before pushing** - Both workers previously told smith to "ensure tests pass" without requiring it to actually run them. The prompts now explicitly instruct smith to run the test suite (go test, dotnet test, npm test, etc.) and fix any failures before committing or pushing, breaking the fix-comments→CI-fails→fix-CI→new-comments loop.
