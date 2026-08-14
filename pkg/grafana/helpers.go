package grafana

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
)

const (
	// Self-hosted (server-admin) endpoints.
	ListUsersPath         = "/api/users"
	GetUserByIDPath       = "/api/users/%d"
	CreateUserPath        = "/api/admin/users"
	DeleteUserPath        = "/api/admin/users/%s"
	ListOrgsPath          = "/api/orgs"
	ListUsersInOrgPath    = "/api/orgs/%s/users"
	AddUserToOrgPath      = "/api/orgs/%s/users"
	RemoveUserFromOrgPath = "/api/orgs/%s/users/%d"
	OrgsForUserPath       = "/api/users/%d/orgs"

	// Cloud-mode endpoints (current-org scope only).
	GetCurrentOrgPath        = "/api/org"
	CurrentOrgUsersPath      = "/api/org/users"
	UpdateCurrentOrgUserPath = "/api/org/users/%d" // Update role - PATCH — to update | DELETE — to remove
	InviteUserPath           = "/api/org/invites"

	// Teams.
	SearchTeamsPath      = "/api/teams/search"
	TeamMembersPath      = "/api/teams/%d/members"
	TeamMemberByUserPath = "/api/teams/%d/members/%d"

	// Service accounts.
	SearchServiceAccountsPath = "/api/serviceaccounts/search"

	// RBAC (Cloud / Enterprise).
	AccessControlRolesPath = "/api/access-control/roles"
	TeamRolesPath          = "/api/access-control/teams/%d/roles"
	TeamRoleByUIDPath      = "/api/access-control/teams/%d/roles/%s"
	SearchTeamRolesPath    = "/api/access-control/teams/roles/search"
)

// ErrTeamMemberAlreadyExists is returned when adding a user who is already on the team.
var ErrTeamMemberAlreadyExists = errors.New("grafana-client: user is already a member of this team")

// ErrTeamMemberNotFound is returned when removing a user who is not on the team.
var ErrTeamMemberNotFound = errors.New("grafana-client: team member not found")

// ErrTeamRoleNotFound is returned when removing an RBAC role that is not assigned to the team.
var ErrTeamRoleNotFound = errors.New("grafana-client: team role not found")

// ErrRBACUnavailable is returned when the RBAC API is not available (OSS without Enterprise).
var ErrRBACUnavailable = errors.New("grafana-client: rbac api unavailable")

// ErrUserAlreadyExists is returned when attempting to create a user that already exists in Grafana.
var ErrUserAlreadyExists = errors.New("grafana-client: user already exists")

// ErrExternalUserLoginDisabled is returned when POST /api/org/invites rejects a
// brand-new external user because the instance's basic login form is disabled.
// This is the Grafana Cloud default (users authenticate via SSO / grafana.com),
// so instance-level invites for non-existing users are rejected with HTTP 400
// "Cannot invite external user when login is disabled." Existing users are added
// to the org without hitting this check.
var ErrExternalUserLoginDisabled = errors.New("grafana-client: cannot invite external user when login is disabled")

func setupPagination(addr *url.URL, paginationVars *PaginationVars) *url.Values {
	if paginationVars == nil {
		return nil
	}

	q := addr.Query()

	if paginationVars.Size != 0 {
		q.Set("perpage", fmt.Sprintf("%d", paginationVars.Size))
	}
	if paginationVars.Page > 0 {
		q.Set("page", fmt.Sprintf("%d", paginationVars.Page))
	}

	return &q
}

// rbacUnavailable maps HTTP 404 from an access-control endpoint to
// ErrRBACUnavailable. The whole /api/access-control route set is absent on OSS
// builds, which answer 404 for every path in it. A missing team is not 404:
// both the per-team GET and the search POST answer 200 with an empty body for
// an unknown team id, so a 404 unambiguously means the API itself is absent.
func rbacUnavailable(err error) bool {
	return strings.Contains(err.Error(), fmt.Sprintf("%d", http.StatusNotFound))
}
