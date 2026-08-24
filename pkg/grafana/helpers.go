package grafana

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strconv"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
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
	SearchTeamRolesPath    = "/api/access-control/teams/roles/search"
)

// ErrTeamMemberAlreadyExists is returned when adding a user who is already on the team.
var ErrTeamMemberAlreadyExists = errors.New("grafana-client: user is already a member of this team")

// ErrTeamMemberNotFound is returned when removing a user who is not on the team.
var ErrTeamMemberNotFound = errors.New("grafana-client: team member not found")

// ErrRBACUnavailable is returned when the RBAC API is not available (OSS without Enterprise).
var ErrRBACUnavailable = errors.New("grafana-client: rbac api unavailable")

// ErrRBACForbidden is returned when the credential cannot read the RBAC API
// (HTTP 403, token without `roles:read`).
var ErrRBACForbidden = errors.New("grafana-client: rbac api forbidden")

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

// normalizeOneBasedPage sets page to 1 when unset (bag token starts at 0).
func normalizeOneBasedPage(pVars *PaginationVars) {
	if pVars != nil && pVars.Page == 0 {
		pVars.Page = 1
	}
}

// nextPageToken returns page+1 as a string when the response was full.
// Callers pass their own page convention (1-based users/teams or 0-based orgs);
// this helper only advances the numeric token.
func nextPageToken(pVars *PaginationVars, pageLen uint64) string {
	if pVars != nil && pVars.Size > 0 && pageLen == pVars.Size {
		return strconv.FormatUint(pVars.Page+1, 10)
	}
	return ""
}

// rbacUnavailable maps HTTP 404 from an access-control endpoint to
// ErrRBACUnavailable. The whole /api/access-control route set is absent on OSS
// builds, which answer 404 for every path in it. A missing team is not 404:
// both the per-team GET and the search POST answer 200 with an empty body for
// an unknown team id, so a 404 unambiguously means the API itself is absent.
func rbacUnavailable(err error) bool {
	return status.Code(err) == codes.NotFound
}

// rbacForbidden maps HTTP 403 from an access-control endpoint to
// ErrRBACForbidden: the API exists but this credential lacks `roles:read`.
// Callers decide what that means — the role type's own List fails closed, while
// the team's secondary role path skips it so team membership keeps syncing.
func rbacForbidden(err error) bool {
	return status.Code(err) == codes.PermissionDenied
}

// ToUser converts a UserByOrgResponse into the shared User shape.
func (ubo UserByOrgResponse) ToUser() User {
	return User{
		ID:            ubo.ID, // Maps userId -> id
		Name:          ubo.Name,
		Login:         ubo.Login,
		Email:         ubo.Email,
		AvatarUrl:     ubo.AvatarUrl,
		IsDisabled:    ubo.IsDisabled,
		LastSeenAt:    ubo.LastSeenAt,
		LastSeenAtAge: ubo.LastSeenAtAge,
		AuthLabels:    ubo.AuthLabels,
		// UserByOrgResponse.IsExternallySynced is a plain bool, so this pointer is non-nil
		// by construction, independent of the API. It carries a meaningful value only on the
		// Cloud List path (/api/org/users, which populates the key); ToUser() is also used on
		// the org Grants path, but there userResource's profile is discarded (only the ID is
		// used), so the emitted is_externally_synced there is never read.
		IsExternallySynced: &ubo.IsExternallySynced,
	}
}

func (r *ServiceAccountSearchResponse) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("empty service account search response")
	}
	if trimmed[0] == '[' {
		var accounts []*ServiceAccount
		if err := json.Unmarshal(trimmed, &accounts); err != nil {
			return err
		}
		r.ServiceAccounts = accounts
		r.TotalCount = len(accounts)
		return nil
	}

	type alias ServiceAccountSearchResponse
	var wrapped alias
	if err := json.Unmarshal(trimmed, &wrapped); err != nil {
		return err
	}
	*r = ServiceAccountSearchResponse(wrapped)
	return nil
}

func (r *rolesListResponse) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		*r = nil
		return nil
	}
	if trimmed[0] == '[' {
		var roles []*Role
		if err := json.Unmarshal(trimmed, &roles); err != nil {
			return err
		}
		*r = roles
		return nil
	}
	// Nested role-detail mocks redirect the list path and return
	// {"permissions":[…], …}. That is not a role catalog.
	var probe struct {
		Permissions json.RawMessage `json:"permissions"`
	}
	if err := json.Unmarshal(trimmed, &probe); err != nil {
		return err
	}
	if probe.Permissions == nil {
		return fmt.Errorf("unexpected roles list object")
	}
	*r = nil
	return nil
}
