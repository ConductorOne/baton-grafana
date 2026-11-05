![Baton Logo](./baton-logo.png)

# baton-grafana
[![Go Reference](https://pkg.go.dev/badge/github.com/conductorone/baton-grafana.svg)](https://pkg.go.dev/github.com/conductorone/baton-grafana)
![ci](https://github.com/conductorone/baton-grafana/actions/workflows/ci.yaml/badge.svg) ![verify](https://github.com/conductorone/baton-grafana/actions/workflows/verify.yaml/badge.svg)

`baton-grafana` is a connector built using the [Baton SDK](https://github.com/conductorone/baton-sdk).
It enables seamless integration with **Grafana** for retrieving users, organizations, and access permissions.

Check out [Baton](https://github.com/conductorone/baton) to learn more about the project.

---

## Table of Contents
- [Getting Started](#getting-started)
- [Installation](#installation)
  - [Homebrew](#homebrew)
  - [Docker](#docker)
  - [From Source](#from-source)
- [Configuration](#configuration)
- [Usage](#usage)
  - [Fetching Resources](#fetching-resources)
  - [Viewing Access Grants](#viewing-access-grants)
  - [Listing Entitlements](#listing-entitlements)
- [Data Model](#data-model)
- [Command Line Options](#command-line-options)
- [Contributing, Support and Issues](#contributing-support-and-issues)

---

## Getting Started

### Requirements
Before using `baton-grafana`, ensure you have:
- A running **Grafana instance** (self-hosted or Grafana Cloud).
- The **Grafana instance URL**, including the scheme (for example, `https://grafana.example.com`).
- Credentials for one of the two authentication methods:
  - **Self-hosted Grafana:** the **username** and **password** of an admin account.
  - **Grafana Cloud:** a **service account token** with the Admin role.

---

## Installation

You can install `baton-grafana` using **Homebrew**, **Docker**, or directly from source.

### Homebrew
    brew install conductorone/baton/baton conductorone/baton/baton-grafana
    baton-grafana
    baton resources

### Docker
    # Self-hosted Grafana
    docker run --rm -v $(pwd):/out \
      -e BATON_HOSTNAME=<instance URL> \
      -e BATON_USERNAME=<username> \
      -e BATON_PASSWORD=<password> \
      public.ecr.aws/conductorone/baton-grafana:latest -f "/out/sync.c1z"

    # Grafana Cloud
    docker run --rm -v $(pwd):/out \
      -e BATON_HOSTNAME=<instance URL> \
      -e BATON_AUTH_METHOD=api-key-flow-group \
      -e BATON_API_TOKEN=<service account token> \
      public.ecr.aws/conductorone/baton-grafana:latest -f "/out/sync.c1z"

    docker run --rm -v $(pwd):/out ghcr.io/conductorone/baton:latest -f "/out/sync.c1z" resources

### From Source
    go install github.com/conductorone/baton/cmd/baton@main
    go install github.com/conductorone/baton-grafana/cmd/baton-grafana@main

    baton-grafana
    baton resources

---

## Configuration

`baton-grafana` supports two authentication methods, and `--auth-method` selects which fields are required:

- **Self-hosted Grafana** (`basic-auth-flow-group`, the default): the username and password of an account with **admin-level access**.
- **Grafana Cloud** (`api-key-flow-group`): a service account token with the Admin role.

**Configuration Fields:**

| Parameter     | Required      | Default                            | Description                                       |
|---------------|---------------|------------------------------------|---------------------------------------------------|
| --hostname    | Yes           | -                                  | The Grafana instance URL, including the scheme (for example, `https://grafana.example.com`). |
| --auth-method | Grafana Cloud | `basic-auth-flow-group` when unset or unrecognized | `api-key-flow-group` for Grafana Cloud, or `basic-auth-flow-group` for self-hosted Grafana (the default). Unless it is `api-key-flow-group`, validation requires `--username` and `--password`. |
| --username    | Self-hosted   | -                                  | Grafana admin username.                           |
| --password    | Self-hosted   | -                                  | Grafana admin password.                           |
| --api-token   | Grafana Cloud | -                                  | Grafana Cloud service account token.              |

A non-empty `--api-token` switches the connector to Grafana Cloud mode (Bearer authentication and the current-organization endpoints), even when `--username` and `--password` are also set. Leave it unset for self-hosted Grafana.

You can also set these values using environment variables. For self-hosted Grafana:

    export BATON_HOSTNAME="https://grafana.example.com"
    export BATON_USERNAME="admin"
    export BATON_PASSWORD="your-password"

For Grafana Cloud:

    export BATON_HOSTNAME="https://your-org.grafana.net"
    export BATON_AUTH_METHOD="api-key-flow-group"
    export BATON_API_TOKEN="your-service-account-token"

---

## Usage

### Fetching Resources
Retrieve all users and organizations:

    baton resources --file=sync.c1z

**Example Output:**

    ID | Display Name     | Resource Type | Parent Resource
    1  | admin            | User          | -
    2  | testUser         | User          | -
    1  | Main Org.        | Organization  | -
    3  | Orgone           | Organization  | -
    5  | Orgthree         | Organization  | -

### Viewing Access Grants
List access grants for users:

    baton grants --file=sync.c1z

**Example Output:**

    ID                   | Resource Type | Resource  | Entitlement      | Principal
    org:1:Admin:user:1   | Organization  | Main Org. | Main Org. Admin  | admin
    org:1:Viewer:user:2  | Organization  | Main Org. | Main Org. Viewer | testUser

### Listing Entitlements
Show permissions available in Grafana:

    baton entitlements --file=sync.c1z

**Example Output:**

    ID           | Display Name     | Resource Type | Resource  | Permission
    org:1:Editor | Main Org. Editor | Organization  | Main Org. | Editor
    org:1:Viewer | Main Org. Viewer | Organization  | Main Org. | Viewer
    org:1:Admin  | Main Org. Admin  | Organization  | Main Org. | Admin

---

## Data Model

`baton-grafana` retrieves the following resources:

- **Users** – Lists all users in Grafana, including their roles.
- **Organizations** – Details organizations and corresponding access grants.
- **Teams** – Grafana teams and their members (`GET /api/teams/search`, `GET /api/teams/{id}/members`).
- **Roles** *(optional)* – Grafana RBAC roles for the IRM and OnCall plugins (for example
  Schedules Editor), including which teams hold those roles (`GET /api/access-control/roles`,
  `GET /api/access-control/teams/{id}/roles`). Requires Grafana Cloud or
  Enterprise; enable the Role resource type in C1 when your instance has access-control.
- **Service accounts** – Grafana service accounts and their organization role.

This information provides insight into user access management in Grafana.

Sync scope and limits:

- On self-hosted Grafana with more than one organization, teams and service accounts sync only from the organization currently selected for the Grafana account the connector signs in with. Organizations, organization roles, and users sync from every organization.
- Users and service accounts whose organization role is `None` (No basic role) still sync, but get no organization grant: the connector models only the Viewer, Editor, and Admin roles.
- Service account sync requires the `GET /api/serviceaccounts/search` endpoint. If it returns `404`, the sync currently completes without service accounts.

---

## Account Provisioning

`baton-grafana` supports account provisioning in both self-hosted and Grafana Cloud modes, but the behavior differs:

- **Self-hosted Grafana:** a new user is created directly (`POST /api/admin/users`) and the generated password is returned to ConductorOne. The connector supports only passwords that ConductorOne stores: set the app's account provisioning **Password storage** to **Save to credential store**. **Do not save password** is not supported.
- **Grafana Cloud:** account creation is invite-based (`POST /api/org/invites`) and no password is returned.

Account deletion also differs by mode:

- **Self-hosted Grafana:** the user is deleted from the instance (`DELETE /api/admin/users/{id}`).
- **Grafana Cloud:** the user is removed from the organization (`DELETE /api/org/users/{id}`). Their Grafana Cloud account is not deleted.

> **Grafana Cloud prerequisite — the basic login form must allow the invite.**
> Grafana Cloud instances ship with the basic login form **disabled** by default (users authenticate through grafana.com / SSO). While it is disabled, Grafana rejects instance-level invites for users who do not yet exist in the instance, and account creation fails with `Cannot invite external user when login is disabled.`
>
> With the service-account token the connector uses:
> - **Users who already exist** in the instance (provisioned earlier via SSO, SCIM, or grafana.com) are added to the organization normally.
> - **Brand-new users** cannot be created until you either enable [SCIM provisioning](https://grafana.com/docs/grafana/latest/setup-grafana/configure-access/configure-scim-provisioning/) (Grafana's recommended path for automatic user lifecycle in Cloud) or enable the basic login form (`disable_login_form = false`).
>
> Managing membership through the grafana.com portal is a separate API and credential (a Grafana Cloud Access Policy token) that the connector's instance service-account token cannot use.

Beyond accounts, `baton-grafana` supports granting and revoking:

- **Organization roles** – Admin / Editor / Viewer on an organization.
- **Team membership** – add/remove a user on a team.

RBAC roles and service accounts are **read-only** (synced, not provisioned).

---

## Command Line Options

Below is a complete list of supported flags along with their corresponding environment variables and default values (if applicable):

| Flag                 | Description                                                                                 | Env Variable          | Default            |
|----------------------|---------------------------------------------------------------------------------------------|-----------------------|--------------------|
| **-f, --file**       | The path to the `.c1z` file used for syncing                                               | `BATON_FILE`           | `sync.c1z`         |
| **-h, --help**       | Show help and usage information                                                            | -                      | -                  |
| **-p, --provisioning** | Enable provisioning support (if supported by the connector)                              | `BATON_PROVISIONING`   | -                  |
| **-v, --version**    | Show version information                                                                   | -                      | -                  |
| **--api-token**      | Grafana Cloud service account token; a non-empty value selects Grafana Cloud mode           | `BATON_API_TOKEN`      | -                  |
| **--auth-method**    | `basic-auth-flow-group` (self-hosted) or `api-key-flow-group` (Grafana Cloud)               | `BATON_AUTH_METHOD`    | `basic-auth-flow-group` when unset or unrecognized |
| **--client-id**      | The client ID used to authenticate with ConductorOne                                       | `BATON_CLIENT_ID`      | -                  |
| **--client-secret**  | The client secret used to authenticate with ConductorOne                                   | `BATON_CLIENT_SECRET`  | -                  |
| **--hostname**       | Grafana instance URL, including the scheme (for example, `https://grafana.example.com`)    | `BATON_HOSTNAME`       | -                  |
| **--log-format**     | The output format for logs: `json` or `console`                                            | `BATON_LOG_FORMAT`     | `json`             |
| **--log-level**      | The log level: `debug`, `info`, `warn`, `error`                                            | `BATON_LOG_LEVEL`      | `info`             |
| **--password**       | Grafana admin password                                                                     | `BATON_PASSWORD`       | -                  |
| **--skip-full-sync** | Skip a full sync (helpful for incremental updates if supported by the connector)           | `BATON_SKIP_FULL_SYNC` | -                  |
| **--ticketing**      | Enable ticketing support (if the connector supports ticketing features)                    | `BATON_TICKETING`      | -                  |
| **--username**       | Grafana admin username                                                                     | `BATON_USERNAME`       | -                  |

> **Tip**: You can combine flags and environment variables. For example, if you set `BATON_HOSTNAME=https://grafana.example.com` as an environment variable, you don’t need to pass `--hostname` explicitly when running commands.

---

# Contributing, Support and Issues

We started Baton because we were tired of taking screenshots and manually
building spreadsheets. We welcome contributions, and ideas, no matter how
small—our goal is to make identity and permissions sprawl less painful for
everyone. If you have questions, problems, or ideas: **Please open a GitHub Issue!**

See [CONTRIBUTING.md](https://github.com/ConductorOne/baton/blob/main/CONTRIBUTING.md) for more details.

---

**Thanks for using baton-grafana!**
