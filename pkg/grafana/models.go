package grafana

import (
	"net/url"

	"github.com/conductorone/baton-sdk/pkg/uhttp"
)

// Client represents a Grafana API client.
type Client struct {
	httpClient *uhttp.BaseHttpClient
	baseUrl    *url.URL

	username string
	password string
}

type Organization struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
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
