package grafana

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"

	"github.com/conductorone/baton-sdk/pkg/uhttp"
)

// GrafanaError represents an error response from the Grafana API.
type GrafanaError struct {
	ErrorMessage string `json:"message"`
	Status       string `json:"status"`
}

// Message implements the uhttp.ErrorResponse interface.
func (e *GrafanaError) Message() string {
	if e.ErrorMessage != "" {
		return e.ErrorMessage
	}
	return "Unknown error from Grafana API"
}

// Client represents a Grafana API client.
type Client struct {
	httpClient *uhttp.BaseHttpClient
	baseUrl    *url.URL

	username string
	password string
	apiToken string // non-empty = Cloud mode (Bearer auth)
}

type Organization struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

type UserOrgResponse struct {
	OrgId   int    `json:"orgId"`
	OrgName string `json:"name"`
	Role    string `json:"role"`
}

type User struct {
	ID            int      `json:"id"`
	UID           string   `json:"uid"`
	Name          string   `json:"name"`
	Login         string   `json:"login"`
	Email         string   `json:"email"`
	AvatarUrl     string   `json:"avatarUrl"`
	IsAdmin       bool     `json:"isAdmin"`
	IsDisabled    bool     `json:"isDisabled"`
	LastSeenAt    string   `json:"lastSeenAt"`
	LastSeenAtAge string   `json:"lastSeenAtAge"`
	AuthLabels    []string `json:"authLabels"`
	// IsExternallySynced reports whether the user's org role is managed by an
	// external IdP (role sync). Only the org-users endpoint returns this field;
	// the global /api/users endpoint omits it entirely from the JSON. It is a
	// pointer so nil ("Grafana did not return it") is distinguishable from an
	// explicit false — verified against the live API: /api/org/users always
	// includes the key, /api/users never does.
	IsExternallySynced *bool `json:"isExternallySynced"`
}

type UserByOrgResponse struct {
	ID                 int      `json:"userId"`
	OrgId              int      `json:"orgId"`
	Email              string   `json:"email"`
	Name               string   `json:"name"`
	AvatarUrl          string   `json:"avatarUrl"`
	Login              string   `json:"login"`
	Role               string   `json:"role"`
	LastSeenAt         string   `json:"lastSeenAt"`
	LastSeenAtAge      string   `json:"lastSeenAtAge"`
	IsDisabled         bool     `json:"isDisabled"`
	AuthLabels         []string `json:"authLabels"`
	IsExternallySynced bool     `json:"isExternallySynced"`
}

// PaginationVars holds pagination parameters for API requests.
type PaginationVars struct {
	Size uint64
	Page uint64
}

// CreateUserRequest represents the request body for creating a new user.
type CreateUserRequest struct {
	Name     string `json:"name"`
	Email    string `json:"email"`
	Login    string `json:"login"`
	Password string `json:"password,omitempty"`
	OrgId    int    `json:"orgId,omitempty"`
}

// AddUserToOrgRequest represents the request body for adding a user to an organization.
type AddUserToOrgRequest struct {
	LoginOrEmail string `json:"loginOrEmail"`
	Role         string `json:"role"`
}

// UpdateOrgUserRoleRequest represents the request body for PATCH /api/org/users/:userId.
type UpdateOrgUserRoleRequest struct {
	Role string `json:"role"`
}

// InviteUserRequest represents the request body for POST /api/org/invites.
type InviteUserRequest struct {
	LoginOrEmail string `json:"loginOrEmail"`
	Name         string `json:"name"`
	Role         string `json:"role"`
	SendEmail    bool   `json:"sendEmail"`
}

// InviteUserResponse represents the response from POST /api/org/invites.
type InviteUserResponse struct {
	Email       string `json:"email"`
	EmailSent   bool   `json:"emailSent"`
	InviteToken string `json:"inviteToken"`
}

// Team is a Grafana team from /api/teams/search or /api/teams/:id.
type Team struct {
	ID          int    `json:"id"`
	UID         string `json:"uid"`
	OrgID       int    `json:"orgId"`
	Name        string `json:"name"`
	Email       string `json:"email"`
	MemberCount int    `json:"memberCount"`
}

// TeamSearchResponse is the paginated payload from GET /api/teams/search.
type TeamSearchResponse struct {
	TotalCount int    `json:"totalCount"`
	Teams      []Team `json:"teams"`
	Page       int    `json:"page"`
	PerPage    int    `json:"perPage"`
}

// TeamMember is a member of a Grafana team from GET /api/teams/:id/members.
type TeamMember struct {
	OrgID      int    `json:"orgId"`
	TeamID     int    `json:"teamId"`
	UserID     int    `json:"userId"`
	Email      string `json:"email"`
	Name       string `json:"name"`
	Login      string `json:"login"`
	AvatarURL  string `json:"avatarUrl"`
	Permission int    `json:"permission"`
}

// AddUserToTeamRequest is the body for POST /api/teams/:id/members.
type AddUserToTeamRequest struct {
	UserID int `json:"userId"`
}

// ServiceAccount is a Grafana service account from /api/serviceaccounts/search.
type ServiceAccount struct {
	ID         int    `json:"id"`
	UID        string `json:"uid"`
	Name       string `json:"name"`
	Login      string `json:"login"`
	OrgID      int    `json:"orgId"`
	IsDisabled bool   `json:"isDisabled"`
	IsExternal bool   `json:"isExternal"`
	Role       string `json:"role"`
	Tokens     int    `json:"tokens"`
}

// ServiceAccountSearchResponse is the paginated payload from GET /api/serviceaccounts/search.
// Live Grafana returns an object with a serviceAccounts array. Some mocks (and
// older servers) return a bare JSON array; UnmarshalJSON accepts both.
type ServiceAccountSearchResponse struct {
	TotalCount      int              `json:"totalCount"`
	ServiceAccounts []ServiceAccount `json:"serviceAccounts"`
	Page            int              `json:"page"`
	PerPage         int              `json:"perPage"`
}

func (r *ServiceAccountSearchResponse) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		return fmt.Errorf("empty service account search response")
	}
	if trimmed[0] == '[' {
		var accounts []ServiceAccount
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

// Role is a Grafana RBAC role from GET /api/access-control/roles.
type Role struct {
	Version     int    `json:"version"`
	UID         string `json:"uid"`
	Name        string `json:"name"`
	DisplayName string `json:"displayName"`
	Description string `json:"description"`
	Group       string `json:"group"`
	Hidden      bool   `json:"hidden"`
	Global      bool   `json:"global"`
}

// rolesListResponse decodes GET /api/access-control/roles. Live Grafana returns
// a JSON array. Some mocks redirect the list path into a role-detail wrapper
// object ({"permissions":[]}); treat that as an empty catalog.
type rolesListResponse []Role

func (r *rolesListResponse) UnmarshalJSON(data []byte) error {
	trimmed := bytes.TrimSpace(data)
	if len(trimmed) == 0 {
		*r = nil
		return nil
	}
	if trimmed[0] == '[' {
		var roles []Role
		if err := json.Unmarshal(trimmed, &roles); err != nil {
			return err
		}
		*r = roles
		return nil
	}
	*r = nil
	return nil
}

// AssignRoleToTeamRequest is the body for POST /api/access-control/teams/:id/roles.
type AssignRoleToTeamRequest struct {
	RoleUID string `json:"roleUid"`
}

// ListRolesForTeamsRequest is the body for
// POST /api/access-control/teams/roles/search.
type ListRolesForTeamsRequest struct {
	TeamIDs []int `json:"teamIds"`
}
