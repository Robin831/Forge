category: Fixed
- **Hearth /login redirects when already authenticated** - Browsing to `/login` while a valid session cookie is attached now serves a 303 redirect to `/` (and the SPA's LoginPage shows a spinner during the auth probe instead of flashing the form). The auth status check moved from `GET /login` to a dedicated `GET /api/auth/status` JSON endpoint. (Forge-jf9h)
