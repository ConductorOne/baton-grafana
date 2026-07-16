package grafana

import (
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
	// external IdP. Only the org-users endpoint returns it; the global /api/users
	// endpoint omits it. A pointer so nil ("not returned") is distinct from false.
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
