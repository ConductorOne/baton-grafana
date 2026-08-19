package grafana

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Client represents a Grafana API client.
type Client struct {
	httpClient *uhttp.BaseHttpClient
	baseUrl    *url.URL

	username string
	password string
	apiToken string // non-empty = Cloud mode (Bearer auth)
}

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
func (c *Client) buildResourceURL(pathTemplate string, args ...any) *url.URL {
	// If no parameters, just use the raw template
	finalPath := pathTemplate
	if len(args) > 0 {
		finalPath = fmt.Sprintf(pathTemplate, args...)
	}
	// ResolveReference merges the base URL and finalPath into an absolute URL.
	return c.baseUrl.ResolveReference(&url.URL{Path: finalPath})
}

// ListOrganizations returns organizations for the current user (paginated, 0-based).
func (c *Client) ListOrganizations(ctx context.Context, pVars *PaginationVars) ([]*Organization, string, annotations.Annotations, error) {
	var organizationsResponse []*Organization
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(ListOrgsPath),
		&organizationsResponse,
		nil,
		pVars,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		return nil, "", annos, err
	}

	return organizationsResponse, nextPageToken(pVars, uint64(len(organizationsResponse))), annos, nil
}

// ListOrgsForUser fetches all organizations for a given Grafana user.
func (c *Client) ListOrgsForUser(ctx context.Context, userID int) ([]*UserByOrgResponse, annotations.Annotations, error) {
	var orgsResponse []*UserByOrgResponse
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(OrgsForUserPath, userID),
		&orgsResponse,
		nil,
		nil,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		return nil, annos, err
	}

	return orgsResponse, annos, nil
}

// ListUsersByOrg fetches all users in a given Grafana organization.
func (c *Client) ListUsersByOrg(ctx context.Context, orgID string) ([]*UserByOrgResponse, annotations.Annotations, error) {
	var usersByOrgResponse []*UserByOrgResponse
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	// Make the request without pagination as the endpoint does not support it
	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(ListUsersInOrgPath, orgID),
		&usersByOrgResponse,
		nil,
		nil,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		return nil, annos, err
	}

	return usersByOrgResponse, annos, nil
}

// ListUsers fetches all users in Grafana (paginated, 1-based).
func (c *Client) ListUsers(ctx context.Context, pVars *PaginationVars) ([]*User, string, annotations.Annotations, error) {
	normalizeOneBasedPage(pVars)
	var usersResponse []*User
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(ListUsersPath),
		&usersResponse,
		nil,
		pVars,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		return nil, "", annos, err
	}

	return usersResponse, nextPageToken(pVars, uint64(len(usersResponse))), annos, nil
}

// doRequest handles HTTP requests with authentication and optional pagination.
// Callers that need extra response handling, such as rate-limit header capture
// with uhttp.WithRatelimitData, pass it through doOpts.
func (c *Client) doRequest(
	ctx context.Context,
	method string,
	urlAddress *url.URL,
	response any,
	data any,
	paginationVars *PaginationVars,
	doOpts ...uhttp.DoOption,
) error {
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

	doOptions := make([]uhttp.DoOption, 0, len(doOpts)+2)
	doOptions = append(doOptions, doOpts...)
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
	// Use a small page size to minimize data transfer. /api/users is 1-based, so
	// page 1 is the first page.
	paginationVars := &PaginationVars{
		Size: 100,
		Page: 1,
	}

	users, _, _, err := c.ListUsers(ctx, paginationVars)
	if err != nil {
		return nil, fmt.Errorf("grafana-client: get user by login or email: %w", err)
	}

	for _, user := range users {
		if user == nil {
			continue
		}
		if strings.EqualFold(user.Login, loginOrEmail) || strings.EqualFold(user.Email, loginOrEmail) {
			return user, nil
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
func (c *Client) ListCurrentOrgUsers(ctx context.Context) ([]*UserByOrgResponse, annotations.Annotations, error) {
	var users []*UserByOrgResponse
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(CurrentOrgUsersPath),
		&users,
		nil,
		nil,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		return nil, annos, fmt.Errorf("grafana-client: list current org users: %w", err)
	}

	return users, annos, nil
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
		// login form is disabled (the Grafana Cloud default). Gate on the 400
		// status (like the 412 check for ErrUserAlreadyExists) plus the message
		// so an unrelated error carrying the same phrase isn't mis-tagged; a
		// reworded message just falls through to the generic error below.
		if strings.Contains(err.Error(), fmt.Sprintf("%d", http.StatusBadRequest)) &&
			strings.Contains(err.Error(), "login is disabled") {
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

// ListTeams calls GET /api/teams/search (page/perpage, 1-based).
// Page 0 is normalized to 1. nextPage is empty when the page was short.
func (c *Client) ListTeams(ctx context.Context, pVars *PaginationVars) ([]*Team, string, annotations.Annotations, error) {
	normalizeOneBasedPage(pVars)
	var resp TeamSearchResponse
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(SearchTeamsPath),
		&resp,
		nil,
		pVars,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		return nil, "", annos, fmt.Errorf("grafana-client: list teams: %w", err)
	}

	return resp.Teams, nextPageToken(pVars, uint64(len(resp.Teams))), annos, nil
}

// ListTeamMembers calls GET /api/teams/{id}/members (not paginated).
func (c *Client) ListTeamMembers(ctx context.Context, teamID int) ([]*TeamMember, annotations.Annotations, error) {
	var members []*TeamMember
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(TeamMembersPath, teamID),
		&members,
		nil,
		nil,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		return nil, annos, fmt.Errorf("grafana-client: list team members: %w", err)
	}

	return members, annos, nil
}

// AddUserToTeam calls POST /api/teams/{id}/members with {"userId": N}.
// HTTP 400 with "already added to this team" maps to ErrTeamMemberAlreadyExists.
func (c *Client) AddUserToTeam(ctx context.Context, teamID, userID int) error {
	req := &AddUserToTeamRequest{UserID: userID}

	err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(TeamMembersPath, teamID), nil, req, nil)
	if err != nil {
		if status.Code(err) == codes.InvalidArgument &&
			strings.Contains(strings.ToLower(err.Error()), "already added to this team") {
			return fmt.Errorf("%w: %w", ErrTeamMemberAlreadyExists, err)
		}
		return fmt.Errorf("grafana-client: add user to team: %w", err)
	}

	return nil
}

// RemoveUserFromTeam calls DELETE /api/teams/{id}/members/{userId}.
// HTTP 404 with "Team member not found" maps to ErrTeamMemberNotFound.
// Other 404 bodies (for example, "Team not found") remain errors.
func (c *Client) RemoveUserFromTeam(ctx context.Context, teamID, userID int) error {
	err := c.doRequest(ctx, http.MethodDelete, c.buildResourceURL(TeamMemberByUserPath, teamID, userID), nil, nil, nil)
	if err != nil {
		if status.Code(err) == codes.NotFound &&
			strings.Contains(err.Error(), "Team member not found") {
			return fmt.Errorf("%w: %w", ErrTeamMemberNotFound, err)
		}
		return fmt.Errorf("grafana-client: remove user from team: %w", err)
	}

	return nil
}

// ListServiceAccounts calls GET /api/serviceaccounts/search (page/perpage, 1-based).
// Page 0 is normalized to 1. nextPage is empty when the page was short.
func (c *Client) ListServiceAccounts(ctx context.Context, pVars *PaginationVars) ([]*ServiceAccount, string, annotations.Annotations, error) {
	normalizeOneBasedPage(pVars)
	var resp ServiceAccountSearchResponse
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(SearchServiceAccountsPath),
		&resp,
		nil,
		pVars,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		return nil, "", annos, fmt.Errorf("grafana-client: list service accounts: %w", err)
	}

	return resp.ServiceAccounts, nextPageToken(pVars, uint64(len(resp.ServiceAccounts))), annos, nil
}

// ListRoles calls GET /api/access-control/roles.
// The endpoint returns all roles in one response and is not paginated.
// HTTP 404 maps to ErrRBACUnavailable (OSS build without access-control).
func (c *Client) ListRoles(ctx context.Context) ([]*Role, annotations.Annotations, error) {
	var roles rolesListResponse
	var rlDesc v2.RateLimitDescription
	annos := annotations.Annotations{}

	err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(AccessControlRolesPath),
		&roles,
		nil,
		nil,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		if rbacUnavailable(err) {
			return nil, annos, fmt.Errorf("%w: %w", ErrRBACUnavailable, err)
		}
		return nil, annos, fmt.Errorf("grafana-client: list roles: %w", err)
	}

	return []*Role(roles), annos, nil
}

// ListRolesForTeams calls POST /api/access-control/teams/roles/search with
// {"teamIds":[...]}. The response is keyed by team id string → []*Role, and an
// unknown team id yields an empty response rather than an error. An empty
// teamIDs slice returns an empty map without calling the API. HTTP 404 maps to
// ErrRBACUnavailable (OSS build without access-control).
func (c *Client) ListRolesForTeams(ctx context.Context, teamIDs []int) (map[string][]*Role, annotations.Annotations, error) {
	annos := annotations.Annotations{}
	if len(teamIDs) == 0 {
		return map[string][]*Role{}, annos, nil
	}

	var rolesByTeam map[string][]*Role
	var rlDesc v2.RateLimitDescription
	req := &ListRolesForTeamsRequest{TeamIDs: teamIDs}
	err := c.doRequest(
		ctx,
		http.MethodPost,
		c.buildResourceURL(SearchTeamRolesPath),
		&rolesByTeam,
		req,
		nil,
		uhttp.WithRatelimitData(&rlDesc),
	)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		if rbacUnavailable(err) {
			return nil, annos, fmt.Errorf("%w: %w", ErrRBACUnavailable, err)
		}
		return nil, annos, fmt.Errorf("grafana-client: list roles for teams: %w", err)
	}
	if rolesByTeam == nil {
		return map[string][]*Role{}, annos, nil
	}
	return rolesByTeam, annos, nil
}
