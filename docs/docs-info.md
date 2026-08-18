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
     current-org members in Cloud). Synced by default.
   - Organizations — Grafana organizations and their role grants (Admin, Editor,
     Viewer). Synced by default.
   - Teams — Grafana teams (`GET /api/teams/search`) and their members
     (`GET /api/teams/{id}/members`). Both endpoints are scoped to the credential's
     **current organization** (self-hosted and Cloud) — unlike self-hosted
     `/api/users` / `/api/orgs`, which are server-admin global. On a multi-org
     self-hosted instance, teams outside the admin's current org are not synced
     and must not be read as deletions. Synced by default — the teams API exists
     on every Grafana edition with the same Admin credential already required for
     users/orgs. Team membership grants are emitted from the team builder; team→
     RBAC role assignments are **not** emitted here (see Roles below).
   - Roles — Grafana RBAC roles from `GET /api/access-control/roles`, filtered to
     the IRM (`plugins:grafana-irm-app:`) and OnCall (`plugins:grafana-oncall-app:`)
     plugin roles (for example `plugins:grafana-irm-app:schedules-editor`,
     "Schedules Editor"). IRM and OnCall ship parallel role catalogs that share
     display names, so the API's `group` disambiguates them ("Admin (IRM)" vs
     "Admin (OnCall)"). The access-control API is only present on Grafana Cloud
     and Enterprise; on OSS builds every `/api/access-control/**` path returns 404.
     The role type is **OptInRequired** — syncing by default would fail List for
     every OSS tenant, and an empty successful List would wipe prior roles in C1.
     Each RBAC call maps its own 404 to `ErrRBACUnavailable`; there is no
     availability probe and no cached state. A missing team is not a 404 (both
     the per-team GET and the search POST answer 200 with an empty body), so the
     404 unambiguously means the API is absent. When the role type is scheduled
     and RBAC is absent, List fails closed. Team→role grants are emitted by
     `roleBuilder.GrantsForResourceType` (`TypeScopedGrants`): the SDK schedules
     that op only when the role type is in the sync, it paginates teams and
     batches `POST /api/access-control/teams/roles/search` per page (same
     current-org team scope as Teams above), and it fails closed on every RBAC
     error (an empty successful emission would wipe prior assignments). Team
     membership still syncs independently when roles are off. Operators enable
     roles in the C1 UI when they have Cloud/Enterprise.
   - Service accounts — `GET /api/serviceaccounts/search`, also scoped to the
     credential's **current organization** (same caveat as Teams on multi-org
     self-hosted). Service accounts do not appear in `GET /api/org/users`, so they
     are a separate resource; their organization role (Viewer/Editor/Admin) is
     emitted as an **immutable** org grant (sync-only — org Grant/Revoke accept
     users only). Synced by default (same Admin credential covers this endpoint).

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
   - **Team membership** — `Grant` / `Revoke` a user on a team
     (`POST /api/teams/{id}/members` with `{"userId"}`,
     `DELETE /api/teams/{id}/members/{userId}`). Re-adding an existing member
     returns HTTP 400 (`User is already added to this team`) and removing an absent
     member returns HTTP 404, both handled as idempotent successes.
   - **Team RBAC roles** — `Grant` / `Revoke` a role on a team
     (`POST /api/access-control/teams/{id}/roles` with `{"roleUid"}`,
     `DELETE /api/access-control/teams/{id}/roles/{roleUid}`). The assign endpoint
     returns HTTP 200 even when the role is already assigned, so the connector
     reads the team's current roles first to report the grant as idempotent.
   - Service accounts are **not** provisioned (read-only).

   > **Note (Grafana Cloud account creation).** Cloud instances ship with the
   > basic login form **disabled** by default. While it is disabled, instance-level
   > invites for users who do **not** yet exist are rejected with
   > `Cannot invite external user when login is disabled.` The connector can still add users
   > who already exist in the instance; brand-new users require SCIM provisioning
   > or enabling the basic login form. See "Additional notes" below.

## Connector credentials

1. **What credentials are needed?**
   - Self-hosted: a Grafana admin **username** + **password**, and the instance
     **hostname**.
   - Grafana Cloud: a **service-account token** (Admin role) and the instance
     **hostname**.

   **Args:**

   | Flag            | Env var             | Mode          | Required                                                |
   | :-------------- | :------------------ | :------------ | :------------------------------------------------------ |
   | `--hostname`    | `BATON_HOSTNAME`    | both          | yes                                                     |
   | `--auth-method` | `BATON_AUTH_METHOD` | both          | selects `basic-auth-flow-group` or `api-key-flow-group` |
   | `--username`    | `BATON_USERNAME`    | self-hosted   | yes (basic-auth group)                                  |
   | `--password`    | `BATON_PASSWORD`    | self-hosted   | yes (basic-auth group)                                  |
   | `--api-token`   | `BATON_API_TOKEN`   | Grafana Cloud | yes (api-key group)                                     |

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

| Operation              | Method + path   | Doc                                                                                                             |
| :--------------------- | :-------------- | :-------------------------------------------------------------------------------------------------------------- |
| Validate (Cloud)       | `GET /api/org`  | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#get-current-organization)       |
| Validate (self-hosted) | `GET /api/orgs` | [Admin Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#search-all-organizations) |

### Users

| Operation                             | Method + path                        | Doc                                                                                                        |
| :------------------------------------ | :----------------------------------- | :--------------------------------------------------------------------------------------------------------- |
| List users (self-hosted)              | `GET /api/users`                     | [User HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/user/#search-users)            |
| List users (Cloud)                    | `GET /api/org/users`                 | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#get-users-in-organization) |
| Get user by login/email (self-hosted) | `GET /api/users` + client-side match | [User HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/user/#search-users)            |
| Create user (self-hosted)             | `POST /api/admin/users`              | [Admin HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/admin/#global-users)          |
| Delete user (self-hosted)             | `DELETE /api/admin/users/{id}`       | [Admin HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/admin/#delete-global-user)    |
| Invite user (Cloud)                   | `POST /api/org/invites`              | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#add-invite)                |

### Organizations

| Operation                     | Method + path                                                        | Doc                                                                                                             |
| :---------------------------- | :------------------------------------------------------------------- | :-------------------------------------------------------------------------------------------------------------- |
| List orgs (self-hosted)       | `GET /api/orgs`                                                      | [Admin Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#search-all-organizations) |
| List org users                | `GET /api/org/users` / `GET /api/orgs/{orgId}/users`                 | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#get-users-in-organization)      |
| Add user to org (role grant)  | `POST /api/org/users` / `POST /api/orgs/{orgId}/users`               | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#add-user-in-organization)       |
| Update org user role          | `PATCH /api/org/users/{id}`                                          | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#updates-the-given-user)         |
| Remove user from org (revoke) | `DELETE /api/org/users/{id}` / `DELETE /api/orgs/{orgId}/users/{id}` | [Org HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/org/#delete-user-in-organization)    |

### Teams

| Operation                   | Method + path                             | Doc                                                                                                        |
| :-------------------------- | :---------------------------------------- | :--------------------------------------------------------------------------------------------------------- |
| Search teams                | `GET /api/teams/search`                   | [Team HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/team/#team-search-with-paging) |
| List team members           | `GET /api/teams/{id}/members`             | [Team HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/team/#get-team-members)        |
| Add team member (grant)     | `POST /api/teams/{id}/members`            | [Team HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/team/#add-team-member)         |
| Remove team member (revoke) | `DELETE /api/teams/{id}/members/{userId}` | [Team HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/team/#remove-member-from-team) |

### RBAC roles (Grafana Cloud / Enterprise)

| Operation                      | Method + path                                           | Doc                                                                                                           |
| :----------------------------- | :------------------------------------------------------ | :------------------------------------------------------------------------------------------------------------ |
| List roles                     | `GET /api/access-control/roles`                         | [RBAC HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/access_control/#get-all-roles)    |
| List roles on a team           | `GET /api/access-control/teams/{id}/roles`              | [RBAC HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/access_control/#list-your-roles)  |
| Assign role to team (grant)    | `POST /api/access-control/teams/{id}/roles`             | [RBAC HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/access_control/#add-team-role)    |
| Remove role from team (revoke) | `DELETE /api/access-control/teams/{id}/roles/{roleUid}` | [RBAC HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/access_control/#remove-team-role) |

### Service accounts

| Operation               | Method + path                     | Doc                                                                                                                                         |
| :---------------------- | :-------------------------------- | :------------------------------------------------------------------------------------------------------------------------------------------ |
| Search service accounts | `GET /api/serviceaccounts/search` | [Service account HTTP API](https://grafana.com/docs/grafana/latest/developers/http_api/serviceaccount/#search-service-accounts-with-paging) |

## Additional notes

### Teams, RBAC roles, and service accounts

The `role` resource is filtered to the IRM and OnCall plugin roles
(`plugins:grafana-irm-app:` and `plugins:grafana-oncall-app:` name prefixes) so
the connector surfaces which teams hold plugin permissions such as **Schedules
Editor** without syncing the full ~300-entry role catalog. Role identity uses the
stable role `name` as the resource ID; the instance-specific `uid` (required by the
assign/unassign endpoints) is stored on the resource profile. Provisioning resolves
the current UID from the stable name before assignment, and revoke uses the UID on
the team's current assignment. Grants are emitted from the team side to avoid
scanning every team per role, and expand through team membership.

Service accounts are read from `GET /api/serviceaccounts/search` because they are
absent from `GET /api/org/users`. They are modeled as service-type accounts and are
not provisioned.

### Grafana Cloud account creation requires an invite-eligible login form

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

Each synced user profile carries `is_externally_synced` and `auth_labels` to indicate
the origin of the user's access (see `connector.mdx` → "Account access origin").

`is_externally_synced` surfaces Grafana's native `isExternallySynced` flag
verbatim — "the user's org role is managed by an external IdP (role sync)" — and only
when Grafana returns it. It is NOT derived from `auth_labels`, which is a different
concept ("the user authenticated via an external module") and is true for every
Grafana Cloud user (all carry `authLabels:["grafana.com"]`); OR-ing it in reported
`true` for every Cloud user and defeated the field's purpose. With the native flag
used verbatim, an instance-managed Cloud admin reports `false` even with
`auth_labels:["grafana.com"]`.

Whether the flag is present depends on the endpoint each mode reads, verified against
the live API: the Cloud org-users endpoint (`/api/org/users`) always returns the key,
so `is_externally_synced` is emitted and mirrors Grafana's value; the self-hosted
global users endpoint (`/api/users`) omits the key entirely, so the native flag on the
`grafana.User` model decodes to `nil` (a `*bool` distinguishes "not returned" from
`false`) and `userResource` leaves the field off the profile rather than deriving a
value from a different concept.

### Org role provisioning for externally synced users

By default Grafana blocks API-level role changes for users whose org role is
managed by an external IdP. Enable **Skip org role sync** for the relevant SSO
provider (`skip_org_role_sync = true`) so C1 can manage those roles. This is a
Cloud-mode consideration; self-hosted basic-auth users are unaffected.
