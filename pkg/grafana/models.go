package grafana

import (
	"net/url"

	"github.com/conductorone/baton-sdk/pkg/uhttp"
)

// Client represents a Grafana API client.
type Client struct {
	httpClient  *uhttp.BaseHttpClient
	baseUrl     *url.URL
	currentUser string

	username     string
	accessToken  string
	password     string
	token        string
	refreshToken string
}

// PaginationVars holds pagination parameters for API requests.
type PaginationVars struct {
	Size uint
	Page uint
}

// ListResponse is a generic type for handling paginated responses.
type ListResponse[T any] struct {
	Items []T `json:"items"`
}

// User represents a Grafana user.
type User struct {
	ID                 int    `json:"userId"`
	OrgId              int    `json:"orgId"`
	Email              string `json:"email"`
	Name               string `json:"name"`
	AvatarUrl          string `json:"avatarUrl"`
	Login              string `json:"login"`
	Role               string `json:"role"`
	LastSeenAt         string `json:"lastSeenAt"`
	LastSeenAtAge      string `json:"lastSeenAtAge"`
	IsDisabled         bool   `json:"isDisabled"`
	AuthLabels         string `json:"authLabels"`
	IsExternallySynced bool   `json:"isExternallySynced"`
}

// Dashboard represents a Grafana dashboard.
type Dashboard struct {
	ID    int    `json:"id"`
	Title string `json:"title"`
	UID   string `json:"uid"`
	URL   string `json:"url"`
}

type BaseResource struct {
	Id string `json:"id"`
}

type Organization struct {
	ID   int    `json:"orgId"`
	Name string `json:"name"`
	Role string `json:"role"`
}
