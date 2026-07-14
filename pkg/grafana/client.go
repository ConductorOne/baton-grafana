package grafana

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
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
)

// ErrUserAlreadyExists is returned when attempting to create a user that already exists in Grafana.
var ErrUserAlreadyExists = errors.New("grafana-client: user already exists")

// ErrExternalUserLoginDisabled is returned when POST /api/org/invites rejects a
// brand-new external user because the instance's basic login form is disabled.
// This is the Grafana Cloud default (users authenticate via SSO / grafana.com),
// so instance-level invites for non-existing users are rejected with HTTP 400
// "Cannot invite external user when login is disabled." Existing users are added
// to the org without hitting this check.
var ErrExternalUserLoginDisabled = errors.New("grafana-client: cannot invite external user when login is disabled")

// NewClient initializes a new Grafana API client.
// When apiToken is non-empty the client operates in Cloud mode (Bearer auth).
// When apiToken is empty the client operates in self-hosted mode (Basic auth).
func NewClient(ctx context.Context, hostname, username, password, apiToken string) (*Client, error) {
	baseUrl, err := url.Parse(hostname)
	if err != nil {
		return nil, err
	}

	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}

	wrapper, err := uhttp.NewBaseHttpClientWithContext(ctx, httpClient)
	if err != nil {
		return nil, err
	}

	return &Client{
		httpClient: wrapper,
		baseUrl:    baseUrl,
		username:   username,
		password:   password,
		apiToken:   apiToken,
	}, nil
}

// IsCloud returns true when the client is configured with a service account token,
// indicating Grafana Cloud mode. In Cloud mode, Bearer auth is used and only the
// current-org endpoint set is available.
func (c *Client) IsCloud() bool {
	return c.apiToken != ""
}

// buildResourceURL constructs an absolute URL by formatting a resource path
// template (like "/api/orgs/%d/users") with optional parameters, then resolving it
// against c.baseURL.
//
// Example:
//
//	If c.baseURL is https://example.com/ and you call:
//	    buildResourceURL("/api/orgs/%d/users", 42)
//	The final URL might be:
//	    https://example.com/api/orgs/42/users
//
// If no parameters are given, the template is used as-is.
// Any errors (like invalid baseURL) can be handled as needed.
func (c *Client) buildResourceURL(pathTemplate string, args ...interface{}) *url.URL {
	// If no parameters, just use the raw template
	finalPath := pathTemplate
	if len(args) > 0 {
		finalPath = fmt.Sprintf(pathTemplate, args...)
	}
	// ResolveReference merges the base URL and finalPath into an absolute URL.
	return c.baseUrl.ResolveReference(&url.URL{Path: finalPath})
}

// ListOrganizations return organizations for the current user.
func (c *Client) ListOrganizations(ctx context.Context, pVars *PaginationVars) ([]Organization, uint64, error) {
	var organizationsResponse []Organization
	var nextPage uint64

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(ListOrgsPath),
		&organizationsResponse,
		nil,
		pVars,
	)
	if err != nil {
		return nil, 0, err
	}

	// Grafana does not provide "nextPage", so we check if we got fewer results than requested
	if uint64(len(organizationsResponse)) == pVars.Size {
		nextPage = pVars.Page + 1
	}

	return organizationsResponse, nextPage, nil
}

// ListOrgsForUser fetches all organizations for a given Grafana user.
func (c *Client) ListOrgsForUser(ctx context.Context, userID int) ([]UserByOrgResponse, error) {
	var orgsResponse []UserByOrgResponse

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(OrgsForUserPath, userID), &orgsResponse, nil, nil)
	if err != nil {
		return nil, err
	}

	return orgsResponse, nil
}

// ListUsersByOrg fetches all users in a given Grafana organization.
func (c *Client) ListUsersByOrg(ctx context.Context, orgID string) ([]UserByOrgResponse, error) {
	var usersByOrgResponse []UserByOrgResponse

	// Make the request without pagination as the endpoint does not support it
	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(ListUsersInOrgPath, orgID), &usersByOrgResponse, nil, nil)
	if err != nil {
		return nil, err
	}

	return usersByOrgResponse, nil
}

// ListUsers fetches all users in Grafana.
func (c *Client) ListUsers(ctx context.Context, pVars *PaginationVars) ([]User, uint64, error) {
	var usersResponse []User
	var nextPage uint64

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(ListUsersPath), &usersResponse, nil, pVars)
	if err != nil {
		return nil, 0, err
	}

	// Grafana does not provide "nextPage", so we check if we got fewer results than requested
	if uint64(len(usersResponse)) == pVars.Size {
		nextPage = pVars.Page + 1
	}

	return usersResponse, nextPage, nil
}

func setupPagination(addr *url.URL, paginationVars *PaginationVars) *url.Values {
	if paginationVars == nil {
		return nil
	}

	q := addr.Query()

	// add page size
	if paginationVars.Size != 0 {
		q.Set("perpage", fmt.Sprintf("%d", paginationVars.Size))
	}

	// add page
	if paginationVars.Page > 0 {
		q.Set("page", fmt.Sprintf("%d", paginationVars.Page))
	}

	return &q
}

// doRequest handles HTTP requests with authentication and optional pagination.
func (c *Client) doRequest(
	ctx context.Context,
	method string,
	urlAddress *url.URL,
	response interface{},
	data interface{},
	paginationVars *PaginationVars,
) error {
	var err error
	l := ctxzap.Extract(ctx)

	reqOptions := []uhttp.RequestOption{
		uhttp.WithContentType("application/json"),
		uhttp.WithAccept("application/json"),
	}

	// Set authentication method — Bearer (Cloud) or Basic (self-hosted)
	if c.IsCloud() {
		reqOptions = append(reqOptions, uhttp.WithHeader("Authorization", "Bearer "+c.apiToken))
	} else {
		authString := fmt.Sprintf("%s:%s", c.username, c.password)
		authEncoded := base64.StdEncoding.EncodeToString([]byte(authString))
		reqOptions = append(reqOptions, uhttp.WithHeader("Authorization", "Basic "+authEncoded))
	}

	if data != nil {
		reqOptions = append(reqOptions, uhttp.WithJSONBody(data))
	}

	q := setupPagination(urlAddress, paginationVars)
	if q != nil {
		urlAddress.RawQuery = q.Encode()
	}

	req, err := c.httpClient.NewRequest(ctx, method, urlAddress, reqOptions...)
	if err != nil {
		return err
	}

	doOptions := []uhttp.DoOption{}
	if response != nil {
		doOptions = append(doOptions, uhttp.WithJSONResponse(response))
	}

	// Add error response handling
	var grafanaError GrafanaError
	doOptions = append(doOptions, uhttp.WithErrorResponse(&grafanaError))

	resp, err := c.httpClient.Do(req, doOptions...)
	if err != nil {
		// Add context logging to HTTP errors
		if l != nil {
			l.Debug("Grafana API error response",
				zap.String("url", urlAddress.String()),
				zap.String("method", method),
				zap.Error(err))
		}
		return err
	}

	defer resp.Body.Close()

	// Log the response status for debugging
	if l != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		l.Debug("Grafana API non-success response",
			zap.String("url", urlAddress.String()),
			zap.String("method", method),
			zap.Int("status", resp.StatusCode))
	}

	return nil
}

// Convert UserByOrg to User.
func (ubo UserByOrgResponse) ToUser() User {
	return User{
		ID:                 ubo.ID, // Maps userId -> id
		Name:               ubo.Name,
		Login:              ubo.Login,
		Email:              ubo.Email,
		AvatarUrl:          ubo.AvatarUrl,
		IsDisabled:         ubo.IsDisabled,
		LastSeenAt:         ubo.LastSeenAt,
		LastSeenAtAge:      ubo.LastSeenAtAge,
		AuthLabels:         ubo.AuthLabels,
		IsExternallySynced: ubo.IsExternallySynced,
	}
}

// GetUserByID fetches a user by their ID.
func (c *Client) GetUserByID(ctx context.Context, userID int) (*User, error) {
	var user User

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(GetUserByIDPath, userID),
		&user,
		nil,
		nil,
	)
	if err != nil {
		return nil, fmt.Errorf("grafana-client: get user by id: %w", err)
	}

	return &user, nil
}

// GetUserByLoginOrEmail searches for a user by login or email in the first page of results.
func (c *Client) GetUserByLoginOrEmail(ctx context.Context, loginOrEmail string) (*User, error) {
	// Use a small page size to minimize data transfer
	paginationVars := &PaginationVars{
		Size: 100,
		Page: 0,
	}

	users, _, err := c.ListUsers(ctx, paginationVars)
	if err != nil {
		return nil, fmt.Errorf("grafana-client: get user by login or email: %w", err)
	}

	for i, user := range users {
		if strings.EqualFold(user.Login, loginOrEmail) || strings.EqualFold(user.Email, loginOrEmail) {
			return &users[i], nil
		}
	}

	return nil, fmt.Errorf("grafana-client: user not found with login or email: %s", loginOrEmail)
}

// CreateUser creates a new user in Grafana.
func (c *Client) CreateUser(ctx context.Context, req *CreateUserRequest) (*User, error) {
	var user User

	err := c.doRequest(
		ctx,
		http.MethodPost,
		c.buildResourceURL(CreateUserPath),
		&user,
		req,
		nil,
	)
	if err != nil {
		// Check if this is a 412 Precondition Failed error, which likely means the user already exists
		if strings.Contains(err.Error(), fmt.Sprintf("%d", http.StatusPreconditionFailed)) {
			return nil, fmt.Errorf("%w: %w", ErrUserAlreadyExists, err)
		}
		return nil, fmt.Errorf("grafana-client: create user: %w", err)
	}

	return &user, nil
}

func (c *Client) DeleteUser(ctx context.Context, userId string) error {
	err := c.doRequest(
		ctx,
		http.MethodDelete,
		c.buildResourceURL(DeleteUserPath, userId),
		nil,
		nil,
		nil,
	)
	if err != nil {
		return fmt.Errorf("grafana-client: delete user: %w", err)
	}

	return nil
}

// GetCurrentOrg fetches the organization the authenticated service account belongs to.
// Cloud-mode equivalent of ListOrganizations() for a single org.
func (c *Client) GetCurrentOrg(ctx context.Context) (*Organization, error) {
	var org Organization

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(GetCurrentOrgPath), &org, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("grafana-client: get current org: %w", err)
	}

	return &org, nil
}

// ListCurrentOrgUsers fetches all members of the current organization.
// No pagination — the endpoint returns the full list in one response.
func (c *Client) ListCurrentOrgUsers(ctx context.Context) ([]UserByOrgResponse, error) {
	var users []UserByOrgResponse

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(CurrentOrgUsersPath), &users, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("grafana-client: list current org users: %w", err)
	}

	return users, nil
}

// UpdateOrgUserRole updates a user's role in the current org via PATCH.
// In Cloud mode this replaces the self-hosted remove+re-add pattern.
func (c *Client) UpdateOrgUserRole(ctx context.Context, userID int, role string) error {
	req := &UpdateOrgUserRoleRequest{Role: role}

	err := c.doRequest(ctx, http.MethodPatch, c.buildResourceURL(UpdateCurrentOrgUserPath, userID), nil, req, nil)
	if err != nil {
		return fmt.Errorf("grafana-client: update org user role: %w", err)
	}

	return nil
}

// AddUserToCurrentOrg adds an existing user (by login or email) to the current org.
// Reuses AddUserToOrgRequest — same request shape as the self-hosted endpoint.
func (c *Client) AddUserToCurrentOrg(ctx context.Context, req *AddUserToOrgRequest) error {
	err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(CurrentOrgUsersPath), nil, req, nil)
	if err != nil {
		return fmt.Errorf("grafana-client: add user to current org: %w", err)
	}

	return nil
}

// RemoveCurrentOrgUser removes a user from the current organization by user ID.
func (c *Client) RemoveCurrentOrgUser(ctx context.Context, userID int) error {
	err := c.doRequest(ctx, http.MethodDelete, c.buildResourceURL(UpdateCurrentOrgUserPath, userID), nil, nil, nil)
	if err != nil {
		return fmt.Errorf("grafana-client: remove current org user: %w", err)
	}

	return nil
}

// InviteUserToOrg sends an invitation to a user to join the current organization.
// Used in Cloud mode for CreateAccount — direct user creation is not available via service account tokens.
func (c *Client) InviteUserToOrg(ctx context.Context, req *InviteUserRequest) (*InviteUserResponse, error) {
	var resp InviteUserResponse

	err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(InviteUserPath), &resp, req, nil)
	if err != nil {
		// Grafana rejects invites for brand-new external users when the basic
		// login form is disabled (the Grafana Cloud default). Tag it so the
		// connector can surface an actionable error instead of a raw 400.
		if strings.Contains(err.Error(), "login is disabled") {
			return nil, fmt.Errorf("%w: %w", ErrExternalUserLoginDisabled, err)
		}
		return nil, fmt.Errorf("grafana-client: invite user to org: %w", err)
	}

	return &resp, nil
}

// AddUserToOrg adds a user to an organization with a specified role.
func (c *Client) AddUserToOrg(ctx context.Context, orgID string, req *AddUserToOrgRequest) error {
	err := c.doRequest(
		ctx,
		http.MethodPost,
		c.buildResourceURL(AddUserToOrgPath, orgID),
		nil,
		req,
		nil,
	)
	if err != nil {
		return fmt.Errorf("grafana-client: add user to org: %w", err)
	}

	return nil
}

// RemoveUserFromOrg removes a user from an organization.
func (c *Client) RemoveUserFromOrg(ctx context.Context, orgID string, userID int) error {
	err := c.doRequest(
		ctx,
		http.MethodDelete,
		c.buildResourceURL(RemoveUserFromOrgPath, orgID, userID),
		nil,
		nil,
		nil,
	)
	if err != nil {
		return fmt.Errorf("grafana-client: remove user from org: %w", err)
	}

	return nil
}
