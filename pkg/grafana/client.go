package grafana

import (
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

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

// doRequest handles HTTP requests with authentication and optional pagination.
func (c *Client) doRequest(
	ctx context.Context,
	method string,
	urlAddress *url.URL,
	response any,
	data any,
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
		ID:            ubo.ID, // Maps userId -> id
		Name:          ubo.Name,
		Login:         ubo.Login,
		Email:         ubo.Email,
		AvatarUrl:     ubo.AvatarUrl,
		IsDisabled:    ubo.IsDisabled,
		LastSeenAt:    ubo.LastSeenAt,
		LastSeenAtAge: ubo.LastSeenAtAge,
		AuthLabels:    ubo.AuthLabels,
		// UserByOrgResponse.IsExternallySynced is a plain bool, so this pointer is non-nil
		// by construction, independent of the API. It carries a meaningful value only on the
		// Cloud List path (/api/org/users, which populates the key); ToUser() is also used on
		// the org Grants path, but there userResource's profile is discarded (only the ID is
		// used), so the emitted is_externally_synced there is never read.
		IsExternallySynced: &ubo.IsExternallySynced,
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
	// Use a small page size to minimize data transfer. /api/users is 1-based, so
	// page 1 is the first page.
	paginationVars := &PaginationVars{
		Size: 100,
		Page: 1,
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
func (c *Client) ListTeams(ctx context.Context, pVars *PaginationVars) ([]Team, uint64, error) {
	var resp TeamSearchResponse

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(SearchTeamsPath), &resp, nil, pVars)
	if err != nil {
		return nil, 0, fmt.Errorf("grafana-client: list teams: %w", err)
	}

	var nextPage uint64
	if uint64(len(resp.Teams)) == pVars.Size {
		nextPage = pVars.Page + 1
	}
	return resp.Teams, nextPage, nil
}

// ListTeamMembers calls GET /api/teams/{id}/members (not paginated).
func (c *Client) ListTeamMembers(ctx context.Context, teamID int) ([]TeamMember, error) {
	var members []TeamMember

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(TeamMembersPath, teamID), &members, nil, nil)
	if err != nil {
		return nil, fmt.Errorf("grafana-client: list team members: %w", err)
	}

	return members, nil
}

// AddUserToTeam calls POST /api/teams/{id}/members with {"userId": N}.
// HTTP 400 with "already added to this team" maps to ErrTeamMemberAlreadyExists.
func (c *Client) AddUserToTeam(ctx context.Context, teamID, userID int) error {
	req := &AddUserToTeamRequest{UserID: userID}

	err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(TeamMembersPath, teamID), nil, req, nil)
	if err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf("%d", http.StatusBadRequest)) &&
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
		if strings.Contains(err.Error(), fmt.Sprintf("%d", http.StatusNotFound)) &&
			strings.Contains(err.Error(), "Team member not found") {
			return fmt.Errorf("%w: %w", ErrTeamMemberNotFound, err)
		}
		return fmt.Errorf("grafana-client: remove user from team: %w", err)
	}

	return nil
}

// ListServiceAccounts calls GET /api/serviceaccounts/search (page/perpage, 1-based).
func (c *Client) ListServiceAccounts(ctx context.Context, pVars *PaginationVars) ([]ServiceAccount, uint64, error) {
	var resp ServiceAccountSearchResponse

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(SearchServiceAccountsPath), &resp, nil, pVars)
	if err != nil {
		return nil, 0, fmt.Errorf("grafana-client: list service accounts: %w", err)
	}

	var nextPage uint64
	if uint64(len(resp.ServiceAccounts)) == pVars.Size {
		nextPage = pVars.Page + 1
	}
	return resp.ServiceAccounts, nextPage, nil
}

// ListRoles calls GET /api/access-control/roles.
// The endpoint returns all roles in one response and is not paginated.
// HTTP 404 maps to ErrRBACUnavailable (OSS build without access-control).
func (c *Client) ListRoles(ctx context.Context) ([]Role, error) {
	var roles []Role

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(AccessControlRolesPath), &roles, nil, nil)
	if err != nil {
		if rbacUnavailable(err) {
			return nil, fmt.Errorf("%w: %w", ErrRBACUnavailable, err)
		}
		return nil, fmt.Errorf("grafana-client: list roles: %w", err)
	}

	return roles, nil
}

// ListTeamRoles calls GET /api/access-control/teams/{id}/roles.
// The endpoint is not paginated and answers 200 with an empty list for an
// unknown team. HTTP 404 maps to ErrRBACUnavailable (OSS build without
// access-control). Prefer ListRolesForTeams on the sync path.
func (c *Client) ListTeamRoles(ctx context.Context, teamID int) ([]Role, error) {
	var roles []Role

	err := c.doRequest(ctx, http.MethodGet, c.buildResourceURL(TeamRolesPath, teamID), &roles, nil, nil)
	if err != nil {
		if rbacUnavailable(err) {
			return nil, fmt.Errorf("%w: %w", ErrRBACUnavailable, err)
		}
		return nil, fmt.Errorf("grafana-client: list team roles: %w", err)
	}

	return roles, nil
}

// ListRolesForTeams calls POST /api/access-control/teams/roles/search with
// {"teamIds":[...]}. The response is keyed by team id string → []Role, and an
// unknown team id yields an empty response rather than an error. An empty
// teamIDs slice returns an empty map without calling the API. HTTP 404 maps to
// ErrRBACUnavailable (OSS build without access-control).
func (c *Client) ListRolesForTeams(ctx context.Context, teamIDs []int) (map[string][]Role, error) {
	if len(teamIDs) == 0 {
		return map[string][]Role{}, nil
	}

	var rolesByTeam map[string][]Role
	req := &ListRolesForTeamsRequest{TeamIDs: teamIDs}
	err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(SearchTeamRolesPath), &rolesByTeam, req, nil)
	if err != nil {
		if rbacUnavailable(err) {
			return nil, fmt.Errorf("%w: %w", ErrRBACUnavailable, err)
		}
		return nil, fmt.Errorf("grafana-client: list roles for teams: %w", err)
	}
	if rolesByTeam == nil {
		return map[string][]Role{}, nil
	}
	return rolesByTeam, nil
}

// AssignRoleToTeam calls POST /api/access-control/teams/{id}/roles with {"roleUid": "..."}.
// The API returns HTTP 200 even when the role is already assigned.
func (c *Client) AssignRoleToTeam(ctx context.Context, teamID int, roleUID string) error {
	req := &AssignRoleToTeamRequest{RoleUID: roleUID}

	err := c.doRequest(ctx, http.MethodPost, c.buildResourceURL(TeamRolesPath, teamID), nil, req, nil)
	if err != nil {
		return fmt.Errorf("grafana-client: assign role to team: %w", err)
	}

	return nil
}

// RemoveRoleFromTeam calls DELETE /api/access-control/teams/{id}/roles/{roleUid}.
// HTTP 404 with "Team role not found" maps to ErrTeamRoleNotFound (idempotent
// revoke). Other 404 bodies (for example, "Team not found") remain errors.
func (c *Client) RemoveRoleFromTeam(ctx context.Context, teamID int, roleUID string) error {
	err := c.doRequest(ctx, http.MethodDelete, c.buildResourceURL(TeamRoleByUIDPath, teamID, roleUID), nil, nil, nil)
	if err != nil {
		if strings.Contains(err.Error(), fmt.Sprintf("%d", http.StatusNotFound)) &&
			strings.Contains(err.Error(), "Team role not found") {
			return fmt.Errorf("%w: %w", ErrTeamRoleNotFound, err)
		}
		return fmt.Errorf("grafana-client: remove role from team: %w", err)
	}

	return nil
}
