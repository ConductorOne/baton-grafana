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
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(ListOrgsPath),
		&organizationsResponse,
		nil,
		pVars,
	)
	if err != nil {
		return nil, "", annos, err
	}

	return organizationsResponse, nextPageToken(pVars, uint64(len(organizationsResponse))), annos, nil
}

// ListOrgsForUser fetches all organizations for a given Grafana user.
func (c *Client) ListOrgsForUser(ctx context.Context, userID int) ([]*UserByOrgResponse, annotations.Annotations, error) {
	var orgsResponse []*UserByOrgResponse
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(OrgsForUserPath, userID),
		&orgsResponse,
		nil,
		nil,
	)
	if err != nil {
		return nil, annos, err
	}

	return orgsResponse, annos, nil
}

// ListUsersByOrg fetches all users in a given Grafana organization.
func (c *Client) ListUsersByOrg(ctx context.Context, orgID string) ([]*UserByOrgResponse, annotations.Annotations, error) {
	var usersByOrgResponse []*UserByOrgResponse

	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(ListUsersInOrgPath, orgID),
		&usersByOrgResponse,
		nil,
		nil,
	)
	if err != nil {
		return nil, annos, err
	}

	return usersByOrgResponse, annos, nil
}

// ListUsers fetches all users in Grafana (paginated, 1-based).
func (c *Client) ListUsers(ctx context.Context, pVars *PaginationVars) ([]*User, string, annotations.Annotations, error) {
	normalizeOneBasedPage(pVars)
	var usersResponse []*User
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(ListUsersPath),
		&usersResponse,
		nil,
		pVars,
	)
	if err != nil {
		return nil, "", annos, err
	}

	return usersResponse, nextPageToken(pVars, uint64(len(usersResponse))), annos, nil
}

// doRequest is the only HTTP call site. It always captures rate-limit headers
// so list and mutate callers can return them to the SDK.
func (c *Client) doRequest(
	ctx context.Context,
	method string,
	urlAddress *url.URL,
	response any,
	data any,
	paginationVars *PaginationVars,
) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)
	annos := annotations.Annotations{}
	var rlDesc v2.RateLimitDescription

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
		return annos, err
	}

	doOptions := make([]uhttp.DoOption, 0, 3)
	doOptions = append(doOptions, uhttp.WithRatelimitData(&rlDesc))
	if response != nil {
		doOptions = append(doOptions, uhttp.WithJSONResponse(response))
	}

	var grafanaError GrafanaError
	doOptions = append(doOptions, uhttp.WithErrorResponse(&grafanaError))

	resp, err := c.httpClient.Do(req, doOptions...)
	annos.WithRateLimiting(&rlDesc)
	if err != nil {
		if l != nil {
			l.Debug("Grafana API error response",
				zap.String("url", urlAddress.String()),
				zap.String("method", method),
				zap.Error(err))
		}
		return annos, err
	}

	defer resp.Body.Close()

	if l != nil && (resp.StatusCode < 200 || resp.StatusCode >= 300) {
		l.Debug("Grafana API non-success response",
			zap.String("url", urlAddress.String()),
			zap.String("method", method),
			zap.Int("status", resp.StatusCode))
	}

	return annos, nil
}

// GetUserByID fetches a user by their ID.
func (c *Client) GetUserByID(ctx context.Context, userID int) (*User, error) {
	var user User

	_, err := c.doRequest(
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
func (c *Client) CreateUser(ctx context.Context, req *CreateUserRequest) (*User, annotations.Annotations, error) {
	var user User

	annos, err := c.doRequest(
		ctx,
		http.MethodPost,
		c.buildResourceURL(CreateUserPath),
		&user,
		req,
		nil,
	)
	if err != nil {
		// 412 maps to InvalidArgument (same bucket as 400); keep the status-code
		// substring so an unrelated 400 is not treated as already-exists.
		if status.Code(err) == codes.InvalidArgument &&
			strings.Contains(err.Error(), fmt.Sprintf("%d", http.StatusPreconditionFailed)) {
			return nil, annos, fmt.Errorf("%w: %w", ErrUserAlreadyExists, err)
		}
		return nil, annos, fmt.Errorf("grafana-client: create user: %w", err)
	}

	return &user, annos, nil
}

func (c *Client) DeleteUser(ctx context.Context, userId string) (annotations.Annotations, error) {
	annos, err := c.doRequest(
		ctx,
		http.MethodDelete,
		c.buildResourceURL(DeleteUserPath, userId),
		nil,
		nil,
		nil,
	)
	if err != nil {
		return annos, fmt.Errorf("grafana-client: delete user: %w", err)
	}

	return annos, nil
}

// GetCurrentOrg fetches the organization the authenticated service account belongs to.
// Cloud-mode equivalent of ListOrganizations() for a single org.
func (c *Client) GetCurrentOrg(ctx context.Context) (*Organization, error) {
	var org Organization

	_, err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(GetCurrentOrgPath), &org, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("grafana-client: get current org: %w", err)
	}

	return &org, nil
}

// ListCurrentOrgUsers fetches all members of the current organization.
// No pagination — the endpoint returns the full list in one response.
func (c *Client) ListCurrentOrgUsers(ctx context.Context) ([]*UserByOrgResponse, annotations.Annotations, error) {
	var users []*UserByOrgResponse
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(CurrentOrgUsersPath),
		&users,
		nil,
		nil,
	)
	if err != nil {
		return nil, annos, fmt.Errorf("grafana-client: list current org users: %w", err)
	}

	return users, annos, nil
}

// UpdateOrgUserRole updates a user's role in the current org via PATCH.
// In Cloud mode this replaces the self-hosted remove+re-add pattern.
func (c *Client) UpdateOrgUserRole(ctx context.Context, userID int, role string) (annotations.Annotations, error) {
	req := &UpdateOrgUserRoleRequest{Role: role}

	annos, err := c.doRequest(ctx, http.MethodPatch, c.buildResourceURL(UpdateCurrentOrgUserPath, userID), nil, req, nil)
	if err != nil {
		return annos, fmt.Errorf("grafana-client: update org user role: %w", err)
	}

	return annos, nil
}

// AddUserToCurrentOrg adds an existing user (by login or email) to the current org.
// Reuses AddUserToOrgRequest — same request shape as the self-hosted endpoint.
func (c *Client) AddUserToCurrentOrg(ctx context.Context, req *AddUserToOrgRequest) (annotations.Annotations, error) {
	annos, err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(CurrentOrgUsersPath), nil, req, nil)
	if err != nil {
		return annos, fmt.Errorf("grafana-client: add user to current org: %w", err)
	}

	return annos, nil
}

// RemoveCurrentOrgUser removes a user from the current organization by user ID.
func (c *Client) RemoveCurrentOrgUser(ctx context.Context, userID int) (annotations.Annotations, error) {
	annos, err := c.doRequest(ctx, http.MethodDelete, c.buildResourceURL(UpdateCurrentOrgUserPath, userID), nil, nil, nil)
	if err != nil {
		return annos, fmt.Errorf("grafana-client: remove current org user: %w", err)
	}

	return annos, nil
}

// InviteUserToOrg sends an invitation to a user to join the current organization.
// Used in Cloud mode for CreateAccount — direct user creation is not available via service account tokens.
func (c *Client) InviteUserToOrg(ctx context.Context, req *InviteUserRequest) (*InviteUserResponse, annotations.Annotations, error) {
	var resp InviteUserResponse

	annos, err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(InviteUserPath), &resp, req, nil)
	if err != nil {
		// Grafana rejects invites for brand-new external users when the basic
		// login form is disabled (the Grafana Cloud default). Gate on the 400
		// status (like the 412 check for ErrUserAlreadyExists) plus the message
		// so an unrelated error carrying the same phrase isn't mis-tagged; a
		// reworded message just falls through to the generic error below.
		if status.Code(err) == codes.InvalidArgument &&
			strings.Contains(err.Error(), "login is disabled") {
			return nil, annos, fmt.Errorf("%w: %w", ErrExternalUserLoginDisabled, err)
		}
		return nil, annos, fmt.Errorf("grafana-client: invite user to org: %w", err)
	}

	return &resp, annos, nil
}

// AddUserToOrg adds a user to an organization with a specified role.
func (c *Client) AddUserToOrg(ctx context.Context, orgID string, req *AddUserToOrgRequest) (annotations.Annotations, error) {
	annos, err := c.doRequest(
		ctx,
		http.MethodPost,
		c.buildResourceURL(AddUserToOrgPath, orgID),
		nil,
		req,
		nil,
	)
	if err != nil {
		return annos, fmt.Errorf("grafana-client: add user to org: %w", err)
	}

	return annos, nil
}

// RemoveUserFromOrg removes a user from an organization.
func (c *Client) RemoveUserFromOrg(ctx context.Context, orgID string, userID int) (annotations.Annotations, error) {
	annos, err := c.doRequest(
		ctx,
		http.MethodDelete,
		c.buildResourceURL(RemoveUserFromOrgPath, orgID, userID),
		nil,
		nil,
		nil,
	)
	if err != nil {
		return annos, fmt.Errorf("grafana-client: remove user from org: %w", err)
	}

	return annos, nil
}

// ListTeams calls GET /api/teams/search (page/perpage, 1-based).
// Page 0 is normalized to 1. nextPage is empty when the page was short.
func (c *Client) ListTeams(ctx context.Context, pVars *PaginationVars) ([]*Team, string, annotations.Annotations, error) {
	normalizeOneBasedPage(pVars)
	var resp TeamSearchResponse
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(SearchTeamsPath),
		&resp,
		nil,
		pVars,
	)
	if err != nil {
		return nil, "", annos, fmt.Errorf("grafana-client: list teams: %w", err)
	}

	return resp.Teams, nextPageToken(pVars, uint64(len(resp.Teams))), annos, nil
}

// ListTeamMembers calls GET /api/teams/{id}/members (not paginated).
func (c *Client) ListTeamMembers(ctx context.Context, teamID int) ([]*TeamMember, annotations.Annotations, error) {
	var members []*TeamMember
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(TeamMembersPath, teamID),
		&members,
		nil,
		nil,
	)
	if err != nil {
		return nil, annos, fmt.Errorf("grafana-client: list team members: %w", err)
	}

	return members, annos, nil
}

// AddUserToTeam calls POST /api/teams/{id}/members with {"userId": N}.
// HTTP 400 with "already added to this team" maps to ErrTeamMemberAlreadyExists.
func (c *Client) AddUserToTeam(ctx context.Context, teamID, userID int) (annotations.Annotations, error) {
	req := &AddUserToTeamRequest{UserID: userID}

	annos, err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(TeamMembersPath, teamID), nil, req, nil)
	if err != nil {
		if status.Code(err) == codes.InvalidArgument &&
			strings.Contains(strings.ToLower(err.Error()), "already added to this team") {
			return annos, fmt.Errorf("%w: %w", ErrTeamMemberAlreadyExists, err)
		}
		return annos, fmt.Errorf("grafana-client: add user to team: %w", err)
	}

	return annos, nil
}

// RemoveUserFromTeam calls DELETE /api/teams/{id}/members/{userId}.
// HTTP 404 with "Team member not found" maps to ErrTeamMemberNotFound.
// Other 404 bodies (for example, "Team not found") remain errors.
func (c *Client) RemoveUserFromTeam(ctx context.Context, teamID, userID int) (annotations.Annotations, error) {
	annos, err := c.doRequest(ctx, http.MethodDelete, c.buildResourceURL(TeamMemberByUserPath, teamID, userID), nil, nil, nil)
	if err != nil {
		if status.Code(err) == codes.NotFound &&
			strings.Contains(err.Error(), "Team member not found") {
			return annos, fmt.Errorf("%w: %w", ErrTeamMemberNotFound, err)
		}
		return annos, fmt.Errorf("grafana-client: remove user from team: %w", err)
	}

	return annos, nil
}

// ListServiceAccounts calls GET /api/serviceaccounts/search (page/perpage, 1-based).
// Page 0 is normalized to 1. nextPage is empty when the page was short.
func (c *Client) ListServiceAccounts(ctx context.Context, pVars *PaginationVars) ([]*ServiceAccount, string, annotations.Annotations, error) {
	normalizeOneBasedPage(pVars)
	var resp ServiceAccountSearchResponse
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(SearchServiceAccountsPath),
		&resp,
		nil,
		pVars,
	)
	if err != nil {
		return nil, "", annos, fmt.Errorf("grafana-client: list service accounts: %w", err)
	}

	return resp.ServiceAccounts, nextPageToken(pVars, uint64(len(resp.ServiceAccounts))), annos, nil
}

// ListRoles calls GET /api/access-control/roles.
// The endpoint returns all roles in one response and is not paginated.
// HTTP 404 maps to ErrRBACUnavailable (OSS build without access-control) and
// HTTP 403 to ErrRBACForbidden (credential without `roles:read`).
func (c *Client) ListRoles(ctx context.Context) ([]*Role, annotations.Annotations, error) {
	var roles rolesListResponse
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(AccessControlRolesPath),
		&roles,
		nil,
		nil,
	)
	if err != nil {
		return nil, annos, wrapRBACError(err, "grafana-client: list roles")
	}

	return []*Role(roles), annos, nil
}

// ListRolesForTeam calls GET /api/access-control/teams/{id}/roles.
// The documented per-team list; an unknown team id yields 200 with an empty
// array rather than 404. HTTP 404 maps to ErrRBACUnavailable (OSS without
// access-control) and HTTP 403 to ErrRBACForbidden (credential without
// `teams.roles:read`).
func (c *Client) ListRolesForTeam(ctx context.Context, teamID int) ([]*Role, annotations.Annotations, error) {
	var roles rolesListResponse
	annos, err := c.doRequest(
		ctx,
		http.MethodGet,
		c.buildResourceURL(TeamRolesPath, teamID),
		&roles,
		nil,
		nil,
	)
	if err != nil {
		return nil, annos, wrapRBACError(err, "grafana-client: list roles for team")
	}
	return []*Role(roles), annos, nil
}
