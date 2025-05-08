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
	ListUsersPath         = "/api/users"
	GetUserByIDPath       = "/api/users/%d"
	CreateUserPath        = "/api/admin/users"
	ListOrgsPath          = "/api/orgs"
	ListUsersInOrgPath    = "/api/orgs/%s/users"
	AddUserToOrgPath      = "/api/orgs/%s/users"
	RemoveUserFromOrgPath = "/api/orgs/%s/users/%d"
)

// ErrUserAlreadyExists is returned when attempting to create a user that already exists in Grafana.
var ErrUserAlreadyExists = errors.New("grafana-client: user already exists")

// NewClient initializes a new Grafana API client.
func NewClient(ctx context.Context, hostname, username, password string) (*Client, error) {
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
	}, nil
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

	// Set authentication method
	authString := fmt.Sprintf("%s:%s", c.username, c.password)
	authEncoded := base64.StdEncoding.EncodeToString([]byte(authString))
	reqOptions = append(reqOptions, uhttp.WithHeader("Authorization", "Basic "+authEncoded))

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
		ID:            ubo.ID, // Maps userId -> id
		Name:          ubo.Name,
		Login:         ubo.Login,
		Email:         ubo.Email,
		AvatarUrl:     ubo.AvatarUrl,
		IsDisabled:    ubo.IsDisabled,
		LastSeenAt:    ubo.LastSeenAt,
		LastSeenAtAge: ubo.LastSeenAtAge,
		AuthLabels:    ubo.AuthLabels,
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
