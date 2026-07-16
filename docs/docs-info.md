# baton-grafana — Setup Guide

Internal setup and reference guide for the Grafana connector. For the
customer-facing documentation see [`connector.mdx`](./connector.mdx); connector
configuration fields live in [`pkg/config/config.go`](../pkg/config/config.go).
The connector is a hand-written Go connector (native `pkg/grafana` HTTP client
over the baton-sdk `uhttp` client), not a declarative `baton-http` connector.

The connector supports two deployment modes, selected by the `--auth-method`
field group:

- **Self-hosted Grafana** (`basic-auth-flow-group`) — Basic auth with an admin
  username/password; uses the server-admin endpoint set.
- **Grafana Cloud** (`api-key-flow-group`) — Bearer auth with a service-account
  token; uses the current-org endpoint set only.

## Connector capabilities

1. **What resources does the connector sync?**
   - Users — every user visible to the credential (global list in self-hosted;
     current-org members in Cloud).
   - Organizations — Grafana organizations and their role grants (Admin, Editor,
     Viewer).

2. **Can the connector provision any resources? If so, which ones?**
   - **Accounts (Users)** — `CreateAccount` and `Delete`.
     - Self-hosted: create via `POST /api/admin/users` (returns a generated
       password); delete via `DELETE /api/admin/users/{id}`.
     - Grafana Cloud: create via `POST /api/org/invites` (invite-based, no
       password returned).
   - **Organization roles** — `Grant` / `Revoke` of the Admin / Editor / Viewer
     role on an organization (`POST /api/org/users`, `PATCH /api/org/users/{id}`,
     `DELETE /api/org/users/{id}` in Cloud; the `/api/orgs/{orgId}/users` variants
     in self-hosted).

   > **Note (Grafana Cloud account creation).** Cloud instances ship with the
   > basic login form **disabled** by default. While it is disabled, instance-level
   > invites for users who do **not** yet exist are rejected with
   > `Cannot invite external user when login is disabled.` C1 can still add users
   > who already exist in the instance; brand-new users require SCIM provisioning
   > or enabling the basic login form. See "Additional notes" below (CXH-2012).

## Connector credentials

1. **What credentials are needed?**
   - Self-hosted: a Grafana admin **username** + **password**, and the instance
     **hostname**.
   - Grafana Cloud: a **service-account token** (Admin role) and the instance
     **hostname**.

   **Args:**

   | Flag | Env var | Mode | Required |
   | :--- | :------ | :--- | :------- |
   | `--hostname` | `BATON_HOSTNAME` | both | yes |
   | `--auth-method` | `BATON_AUTH_METHOD` | both | selects `basic-auth-flow-group` or `api-key-flow-group` |
   | `--username` | `BATON_USERNAME` | self-hosted | yes (basic-auth group) |
   | `--password` | `BATON_PASSWORD` | self-hosted | yes (basic-auth group) |
   | `--api-token` | `BATON_API_TOKEN` | Grafana Cloud | yes (api-key group) |

2. **For each credential:**
   - *How does a user create or look up the credential?*
     - Self-hosted: use an existing Grafana admin login (username/password).
     - Grafana Cloud: in the instance, go to **Administration → Users and access →
       Service accounts**, create a service account with the **Admin** role, and
       add a service-account token.
   - *Does it need specific scopes/permissions?* Grafana uses a role model, not
     OAuth scopes. The credential must have **Admin** on the organization(s) being
     synced/provisioned so it can read users/orgs and manage org membership.
   - *Different scopes to sync vs provision?* No separate scope — the same Admin
     role covers read (sync) and write (provisioning). Account creation in Cloud
     additionally depends on the instance login-form configuration (see notes).
   - *What access is needed to CREATE the credential?* Org Admin (to create a
     service-account token) or server admin (self-hosted).

## Resource reference (API doc links)

API doc root: <https://grafana.com/docs/grafana/latest/developers/http_api/>

### Authentication

| Operation | Method + path | Doc |
| :-------- | :------------ | :-- |
| Validate (Cloud) | `GET /api/org` | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#get-current-organization) |
| Validate (self-hosted) | `GET /api/orgs` | [Admin Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#search-all-organizations) |

### Users

| Operation | Method + path | Doc |
| :-------- | :------------ | :-- |
| List users (self-hosted) | `GET /api/users` | [User HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/user/#search-users) |
| List users (Cloud) | `GET /api/org/users` | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#get-users-in-organization) |
| Get user by login/email (self-hosted) | `GET /api/users` + client-side match | [User HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/user/#search-users) |
| Create user (self-hosted) | `POST /api/admin/users` | [Admin HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/admin/#global-users) |
| Delete user (self-hosted) | `DELETE /api/admin/users/{id}` | [Admin HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/admin/#delete-global-user) |
| Invite user (Cloud) | `POST /api/org/invites` | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#add-invite) |

### Organizations

| Operation | Method + path | Doc |
| :-------- | :------------ | :-- |
| List orgs (self-hosted) | `GET /api/orgs` | [Admin Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#search-all-organizations) |
| List org users | `GET /api/org/users` / `GET /api/orgs/{orgId}/users` | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#get-users-in-organization) |
| Add user to org (role grant) | `POST /api/org/users` / `POST /api/orgs/{orgId}/users` | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#add-user-in-organization) |
| Update org user role | `PATCH /api/org/users/{id}` | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#updates-the-given-user) |
| Remove user from org (revoke) | `DELETE /api/org/users/{id}` / `DELETE /api/orgs/{orgId}/users/{id}` | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#delete-user-in-organization) |

## Additional notes

### Grafana Cloud account creation requires an invite-eligible login form (CXH-2012)

In Cloud mode, `CreateAccount` sends an org invite (`POST /api/org/invites`).
Grafana Cloud instances ship with the basic login form **disabled** by default
(`disable_login_form = true`; users authenticate via grafana.com / SSO). Grafana
only enforces this check for users who do **not** already exist in the instance:

- **Existing users** (already provisioned via SSO, SCIM, or grafana.com) are added
  to the organization normally — the invite path skips the login-form check.
- **Brand-new external users** are rejected with HTTP 400
  `Cannot invite external user when login is disabled.`

The connector detects this response (`grafana.ErrExternalUserLoginDisabled`) and
returns a terminal, actionable error rather than an opaque 400. To provision
brand-new users, either:

- enable [SCIM provisioning](https://grafana.com/docs/grafana/latest/setup-grafana/configure-access/configure-scim-provisioning/)
  (Grafana's recommended path for automatic user lifecycle in Cloud), so the IdP
  provisions the user before C1 assigns organization roles; or
- enable the basic login form on the instance (`disable_login_form = false`).

The grafana.com portal can manage Cloud org membership, but it is a separate API
(`https://grafana.com/api`) authenticated with a Grafana Cloud Access Policy
token — a different credential than the instance service-account token the
connector uses, so it is not an option from within the connector.

### Access origin attributes

Each synced user profile carries `is_externally_synced` and `auth_labels` to
indicate the origin of the user's access (see `connector.mdx` → "Account access
origin"). `is_externally_synced` is Grafana's native `isExternallySynced` flag
(external role sync). It is surfaced only when Grafana returns it: the Cloud
org-users endpoint always does, while the self-hosted global `/api/users`
endpoint omits it, so the attribute is left off self-hosted profiles rather than
derived from `auth_labels` (a different concept — authentication provenance).

### Org role provisioning for externally synced users

By default Grafana blocks API-level role changes for users whose org role is
managed by an external IdP. Enable **Skip org role sync** for the relevant SSO
provider (`skip_org_role_sync = true`) so C1 can manage those roles. This is a
Cloud-mode consideration; self-hosted basic-auth users are unaffected.
