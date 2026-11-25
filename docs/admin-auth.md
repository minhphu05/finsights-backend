# Admin console authentication

Admin principals live in `finsights_auth.admin_accounts`, separately from end-user accounts and Google identities.

## Bootstrap

Set `ADMIN_BOOTSTRAP_USERNAME=adminbase` and `ADMIN_BOOTSTRAP_PASSWORD` in the backend Kubernetes Secret before rolling out AuthService. At startup, AuthService inserts this account only if that username is absent; it hashes the password with bcrypt and does not reset a password already in the database. The bootstrap password is not part of the repository or Helm values.

Admin usernames are normalized to lowercase and must be 3–64 characters from `a-z`, `0-9`, `.`, `_`, and `-`. Passwords must be 16–72 bytes. Five consecutive incorrect attempts lock the account for 15 minutes. A successful login clears the counter and updates `last_login_at`.

## HTTP endpoints

| Method | Path | Access | Purpose |
|---|---|---|---|
| `POST` | `/api/v1/admin/auth/login` | Public; username/password required | Authenticate an admin and receive a short-lived admin JWT |
| `GET` | `/api/v1/admin/auth/me` | Admin JWT | Validate the current session and return the admin identity |
| `GET` | `/api/v1/admin/admins` | Admin JWT | List admin usernames, status, creator, and login timestamps |
| `POST` | `/api/v1/admin/admins` | Admin JWT | Create an admin; body contains `username` and `password` |

Admin tokens carry `token_use=admin` and role `admin`. User-password and Google-sign-in tokens cannot use admin endpoints. The gateway forwards admin tokens to AuthService, whose gRPC interceptor also checks admin claims before listing or creating accounts. Password hashes and login request bodies are never returned by list or create responses.

The initial admin account is not deleted or overwritten by routine service starts. Additional admins are created by an authenticated admin; creation records the actor ID in `created_by_admin_id`.
