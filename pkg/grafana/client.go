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
	BaseDomain = "localhost"
	Protocol   = "http"
	Port       = "3000"

	ListUsersPath      = "/api/users"
	ListOrgsPath       = "/api/orgs"
	ListUsersInOrgPath = "/api/orgs/%s/users"
)

type CredentialsReq struct {
	Username string `json:"username"`
	Password string `json:"password"`
}

type TokenResp struct {
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
}

// NewClient initializes a new Grafana API client.
func NewClient(ctx context.Context, username, password, accessToken string) (*Client, error) {
	base := &url.URL{
		Scheme: Protocol,
		Host:   fmt.Sprintf("%s:%s", BaseDomain, Port),
	}

	httpClient, err := uhttp.NewClient(ctx, uhttp.WithLogger(true, ctxzap.Extract(ctx)))
	if err != nil {
		return nil, err
	}

	wrapper, err := uhttp.NewBaseHttpClientWithContext(ctx, httpClient)
	if err != nil {
		return nil, err
	}

	reqOptions := []uhttp.RequestOption{
		uhttp.WithContentType("application/json"),
		uhttp.WithAccept("application/json"),
	}

	// Conditionally set authentication method
	if accessToken != "" {
		reqOptions = append(reqOptions, uhttp.WithBearerToken(accessToken))
	} else if username != "" && password != "" {
		authString := fmt.Sprintf("%s:%s", username, password)
		authEncoded := base64.StdEncoding.EncodeToString([]byte(authString))
		reqOptions = append(reqOptions, uhttp.WithHeader("Authorization", "Basic "+authEncoded))
	}

	urlAddress := base.ResolveReference(&url.URL{Path: "/api/user"})

	req, err := wrapper.NewRequest(ctx, http.MethodGet, urlAddress, reqOptions...)
	if err != nil {
		return nil, err
	}

	data := &TokenResp{}
	doOptions := []uhttp.DoOption{
		uhttp.WithJSONResponse(data),
	}
	resp, err := wrapper.Do(req, doOptions...)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	return &Client{
		httpClient:   wrapper,
		baseUrl:      base,
		username:     username,
		accessToken:  accessToken,
		password:     password,
		token:        data.Token,
		refreshToken: data.RefreshToken,
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

	return organizationsResponse, nextPage, nil //
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

// GetCurrentUser fetches information about the currently logged-in user.
// func (c *Client) GetCurrentUser(ctx context.Context) (*User, error) {
// 	var userResponse User
// 	err := c.doRequest(ctx, http.MethodGet, "/api/user", &userResponse, nil, nil)
// 	if err != nil {
// 		return nil, err
// 	}
// 	return &userResponse, nil
// }

// SetCurrentUser sets the current user for the client.
func (c *Client) SetCurrentUser(ctx context.Context, username string) error {
	c.currentUser = username

	return nil
}

// ListDashboards fetches all dashboards in Grafana.
// func (c *Client) ListDashboards(ctx context.Context, pagination PaginationVars) ([]Dashboard, error) {
// 	var response ListResponse[Dashboard]
// 	err := c.doRequest(ctx, http.MethodGet, c.composeURL("/api/search"), &response, nil, &pagination)
// 	if err != nil {
// 		return nil, err
// 	}
// 	return response.Items, nil
// }

func setupPagination(ctx context.Context, addr *url.URL, paginationVars *PaginationVars) *url.Values {
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
	l := ctxzap.Extract(ctx)
	l.Info(fmt.Sprintf("DO REQUEST: urlurlAddress: %s <|||||", urlAddress))
	var err error

	reqOptions := []uhttp.RequestOption{
		uhttp.WithContentType("application/json"),
		uhttp.WithAccept("application/json"),
	}
	reqOptions = append(reqOptions, uhttp.WithHeader("Authorization", "Basic YWRtaW46YWRtaW4="))

	if data != nil {
		reqOptions = append(reqOptions, uhttp.WithJSONBody(data))
	}

	q := setupPagination(ctx, urlAddress, paginationVars)
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

// Convert UserByOrg to User
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
