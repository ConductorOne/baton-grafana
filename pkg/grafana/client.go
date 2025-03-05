package grafana

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"

	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
)

const (
	ListUsersPath      = "/api/users"
	ListOrgsPath       = "/api/orgs"
	ListUsersInOrgPath = "/api/orgs/%s/users"
)

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

	resp, err := c.httpClient.Do(req, doOptions...)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

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
