![Baton Logo](./baton-logo.png)

# baton-grafana  
[![Go Reference](https://pkg.go.dev/badge/github.com/conductorone/baton-grafana.svg)](https://pkg.go.dev/github.com/conductorone/baton-grafana)  
![main ci](https://github.com/conductorone/baton-grafana/actions/workflows/main.yaml/badge.svg)

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
- A running **Grafana instance**.
- An **admin account** with sufficient privileges.
- A valid **username** and **password** for authentication.
- The **Grafana domain URL** for API access.

---

## Installation

You can install `baton-grafana` using **Homebrew**, **Docker**, or directly from source.

### Homebrew
    brew install conductorone/baton/baton conductorone/baton/baton-grafana
    baton-grafana
    baton resources

### Docker
    docker run --rm -v $(pwd):/out \
      -e BATON_HOSTNAME=<hostname> \
      -e BATON_USERNAME=<username> \
      -e BATON_PASSWORD=<password> \
      ghcr.io/conductorone/baton-grafana:latest -f "/out/sync.c1z"

    docker run --rm -v $(pwd):/out ghcr.io/conductorone/baton:latest -f "/out/sync.c1z" resources

### From Source
    go install github.com/conductorone/baton/cmd/baton@main
    go install github.com/conductorone/baton-grafana/cmd/baton-grafana@main

    baton-grafana
    baton resources

---

## Configuration

`baton-grafana` requires a **Grafana username and password** for authentication. These credentials must have **admin-level access** to retrieve organization, user, and permission data.

**Configuration Fields:**

| Parameter     | Required | Default                 | Description                                       |
|---------------|----------|-------------------------|---------------------------------------------------|
| --hostname    | No       | `http://localhost:3000` | The Grafana server hostname.                      |
| --username    | Yes      | -                       | Grafana admin username.                           |
| --password    | Yes      | -                       | Grafana admin password.                           |

You can also set these values using environment variables:

    export BATON_HOSTNAME="http://example.com"
    export BATON_USERNAME="admin"
    export BATON_PASSWORD="your-password"

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

This information provides insight into user access management in Grafana.

---

## Command Line Options

Below is a complete list of supported flags along with their corresponding environment variables and default values (if applicable):

| Flag                 | Description                                                                                 | Env Variable          | Default            |
|----------------------|---------------------------------------------------------------------------------------------|-----------------------|--------------------|
| **-f, --file**       | The path to the `.c1z` file used for syncing                                               | `BATON_FILE`           | `sync.c1z`         |
| **-h, --help**       | Show help and usage information                                                            | -                      | -                  |
| **-p, --provisioning** | Enable provisioning support (if supported by the connector)                              | `BATON_PROVISIONING`   | -                  |
| **-v, --version**    | Show version information                                                                   | -                      | -                  |
| **--client-id**      | The client ID used to authenticate with ConductorOne                                       | `BATON_CLIENT_ID`      | -                  |
| **--client-secret**  | The client secret used to authenticate with ConductorOne                                   | `BATON_CLIENT_SECRET`  | -                  |
| **--hostname**       | Grafana hostname (e.g., `http://localhost:3000`)                                           | `BATON_HOSTNAME`       | `http://localhost:3000`  |
| **--log-format**     | The output format for logs: `json` or `console`                                            | `BATON_LOG_FORMAT`     | `json`             |
| **--log-level**      | The log level: `debug`, `info`, `warn`, `error`                                            | `BATON_LOG_LEVEL`      | `info`             |
| **--password**       | Grafana admin password                                                                     | `BATON_PASSWORD`       | -                  |
| **--skip-full-sync** | Skip a full sync (helpful for incremental updates if supported by the connector)           | `BATON_SKIP_FULL_SYNC` | -                  |
| **--ticketing**      | Enable ticketing support (if the connector supports ticketing features)                    | `BATON_TICKETING`      | -                  |
| **--username**       | Grafana admin username                                                                     | `BATON_USERNAME`       | -                  |

> **Tip**: You can combine flags and environment variables. For example, if you set `BATON_HOSTNAME=my-grafana.domain` as environment variable, you don’t need to pass `--hostname` explicitly when running commands.

---

# Contributing, Support and Issues

We started Baton because we were tired of taking screenshots and manually
building spreadsheets. We welcome contributions, and ideas, no matter how
small—our goal is to make identity and permissions sprawl less painful for
everyone. If you have questions, problems, or ideas: **Please open a GitHub Issue!**

See [CONTRIBUTING.md](https://github.com/ConductorOne/baton/blob/main/CONTRIBUTING.md) for more details.

---

**Thanks for using baton-grafana!**
