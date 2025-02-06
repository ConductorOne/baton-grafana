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
func NewClient(ctx context.Context, hostname, protocol, username, password string) (*Client, error) {

	base := &url.URL{
		Scheme: protocol,
		Host:   hostname,
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
		baseUrl:    base,
		username:   username,
		password:   password,
	}, nil
}

// composeURL builds the full API endpoint URL.
func (c *Client) composeURL(endpoint string, params ...interface{}) *url.URL {
	path := endpoint
	if len(params) > 0 {
		path = fmt.Sprintf(endpoint, params...)
	}
	return c.baseUrl.ResolveReference(&url.URL{Path: path})
}

// ListOrganizations return organizations for the current user.
func (c *Client) ListOrganizations(ctx context.Context, pVars *PaginationVars) ([]Organization, uint64, error) {
	var organizationsResponse []Organization
	var nextPage uint64

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.composeURL(ListOrgsPath),
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
	err := c.doRequest(ctx, http.MethodGet, c.composeURL(ListUsersInOrgPath, orgID), &usersByOrgResponse, nil, nil)
	if err != nil {
		return nil, err
	}

	return usersByOrgResponse, nil
}

// ListUsers fetches all users in Grafana.
func (c *Client) ListUsers(ctx context.Context, pVars *PaginationVars) ([]User, uint64, error) {
	var usersResponse []User
	var nextPage uint64

	err := c.doRequest(ctx, http.MethodGet, c.composeURL(ListUsersPath), &usersResponse, nil, pVars)
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
	if c.username != "" && c.password != "" {
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
