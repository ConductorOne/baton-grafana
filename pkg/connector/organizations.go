package connector

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	ent "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

const (
	roleViewer = "Viewer"
	roleEditor = "Editor"
	roleAdmin  = "Admin"
)

var userRoles = []string{roleViewer, roleEditor, roleAdmin}

type orgBuilder struct {
	resourceType *v2.ResourceType
	client       *grafana.Client
}

func (o *orgBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeOrg
}

// Create a new connector resource for an grafana organization.
func orgResource(org grafana.Organization) (*v2.Resource, error) {
	resource, err := rs.NewResource(
		titleCase(org.Name),
		resourceTypeOrg,
		org.ID,
	)

	if err != nil {
		return nil, err
	}

	return resource, nil
}

// List returns all the organizations.
func (o *orgBuilder) List(ctx context.Context, _ *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	if o.client.IsCloud() {
		return o.listCloud(ctx)
	}
	return o.listSelfHosted(ctx, pToken)
}

// listCloud fetches only the current org (Cloud mode — single org scope).
// ID stability: org.ID from GET /api/org is the same numeric Grafana org ID as from GET /api/orgs.
func (o *orgBuilder) listCloud(ctx context.Context) ([]*v2.Resource, string, annotations.Annotations, error) {
	org, err := o.client.GetCurrentOrg(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: cloud: failed to get current org: %w", err)
	}

	resource, err := orgResource(*org)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: cloud: failed to create org resource: %w", err)
	}

	return []*v2.Resource{resource}, "", nil, nil
}

// listSelfHosted is the original List logic for self-hosted Grafana — unchanged.
func (o *orgBuilder) listSelfHosted(ctx context.Context, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	// Parse pagination token. If Token is an empty string, the function returns 0.
	bag, page, err := parsePageToken(pToken, &v2.ResourceId{ResourceType: resourceTypeOrg.Id})
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to parse page token: %w", err)
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	}

	// Fetch organizations from Grafana
	orgs, numNextPage, err := o.client.ListOrganizations(ctx, &paginationOpts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list organizations: %w", err)
	}

	// Determine next page token
	var pageToken string
	if numNextPage > 0 {
		pageToken = strconv.FormatUint(numNextPage, 10)
	}

	next, err := bag.NextToken(pageToken)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to generate next page token: %w", err)
	}

	// Iterate over organizations and filter valid ones
	resources := make([]*v2.Resource, 0, len(orgs))
	for _, org := range orgs {
		// Convert organization to a v2.Resource
		resource, err := orgResource(org)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to create resource for org %s: %w", org.Name, err)
		}

		resources = append(resources, resource)
	}

	return resources, next, nil, nil
}

// Entitlements returns a slice of entitlements for possible user roles under organization (Viewer, Editor, Admin).
func (o *orgBuilder) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	// Preallocate slice for efficiency
	entitlements := make([]*v2.Entitlement, 0, len(userRoles))

	for _, role := range userRoles {
		// Generate display name and description
		displayName := fmt.Sprintf("%s %s", resource.DisplayName, role)
		description := fmt.Sprintf("%s role in %s Grafana organization", titleCase(role), resource.DisplayName)

		// Define entitlement options
		entitlementOptions := []ent.EntitlementOption{
			ent.WithGrantableTo(resourceTypeUser),
			ent.WithDisplayName(displayName),
			ent.WithDescription(description),
		}

		// Append new entitlement to the slice
		entitlements = append(entitlements, ent.NewPermissionEntitlement(resource, role, entitlementOptions...))
	}

	return entitlements, "", nil, nil
}

// Grants returns a slice of grants for each user and their set role under organization.
func (o *orgBuilder) Grants(ctx context.Context, parentResource *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	var usersByOrgResponse []grafana.UserByOrgResponse
	var err error

	if o.client.IsCloud() {
		// Cloud mode: GET /api/org/users — no orgID param needed, no pagination.
		// ID stability: UserByOrgResponse.ID (json:"userId") is the same numeric Grafana user ID
		// as User.ID from the self-hosted path. Grant IDs are therefore unchanged.
		usersByOrgResponse, err = o.client.ListCurrentOrgUsers(ctx)
		if err != nil {
			return nil, "", nil, fmt.Errorf("grafana-connector: cloud: failed to list current org users: %w", err)
		}
	} else {
		// Self-hosted mode: original behavior unchanged
		usersByOrgResponse, err = o.client.ListUsersByOrg(ctx, parentResource.Id.Resource)
		if err != nil {
			return nil, "", nil, fmt.Errorf("grafana-connector: failed to list users under organization %s: %w", parentResource.Id.Resource, err)
		}
	}

	grants := make([]*v2.Grant, 0, len(usersByOrgResponse))

	// Iterate through users and create grants — identical for both modes
	for _, userByOrg := range usersByOrgResponse {
		// Skip users with invalid roles
		if !slices.Contains(userRoles, userByOrg.Role) {
			continue
		}

		// Convert UserByOrg to User only when needed
		user := userByOrg.ToUser()
		ur, err := userResource(&user)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to generate user resource for %s: %w", user.Email, err)
		}

		// Append grant to the slice
		grants = append(grants, grant.NewGrant(parentResource, userByOrg.Role, ur.Id))
	}

	return grants, "", nil, nil
}

// Grant adds a user to an organization with the specified role.
func (o *orgBuilder) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)
	
	// Verify the principal is a user
	if principal.Id.ResourceType != resourceTypeUser.Id {
		return nil, fmt.Errorf("grafana-connector: principal must be a user, got %s", principal.Id.ResourceType)
	}

	// Get the organization ID from the entitlement resource
	orgID, err := strconv.Atoi(entitlement.Resource.Id.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: invalid organization ID %s: %w", entitlement.Resource.Id.Resource, err)
	}

	// Parse the role from the entitlement ID, which is in format "resourceType:resourceId:permission"
	parts := strings.Split(entitlement.Id, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("grafana-connector: invalid entitlement ID format %s", entitlement.Id)
	}

	// The role is the last part of the entitlement ID
	role := parts[2]

	// Verify the role is valid
	if !slices.Contains(userRoles, role) {
		return nil, fmt.Errorf("grafana-connector: invalid role %s", role)
	}

	// Convert user ID to int for API calls
	userID, err := strconv.Atoi(principal.Id.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: invalid user ID %s: %w", principal.Id.Resource, err)
	}

	if o.client.IsCloud() {
		return o.grantCloud(ctx, l, principal, userID, orgID, role)
	}
	return o.grantSelfHosted(ctx, l, userID, orgID, role)
}

// grantCloud handles Grant in Cloud mode.
// Strategy:
//  1. List current org users to check membership.
//  2. Same role → GrantAlreadyExists.
//  3. Different role → PATCH (no remove+re-add needed, more efficient).
//  4. Not in org → POST /api/org/users using email from UserTrait (avoids
//     GET /api/users/:id which is forbidden in Cloud mode).
func (o *orgBuilder) grantCloud(ctx context.Context, l *zap.Logger, principal *v2.Resource, userID, orgID int, role string) (annotations.Annotations, error) {
	l.Debug("Cloud mode: granting org membership", zap.Int("user_id", userID), zap.Int("org_id", orgID), zap.String("role", role))

	currentUsers, err := o.client.ListCurrentOrgUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: cloud: failed to list org users for grant: %w", err)
	}

	for _, cu := range currentUsers {
		if cu.ID == userID {
			if cu.Role == role {
				// Already has the requested role
				return annotations.New(&v2.GrantAlreadyExists{}), nil
			}
			// Role differs — update via PATCH (single call, no remove+re-add)
			l.Debug("Cloud mode: updating user role via PATCH", zap.Int("user_id", userID), zap.String("new_role", role))
			if err = o.client.UpdateOrgUserRole(ctx, userID, role); err != nil {
				if cu.IsExternallySynced && isExternallySyncedRoleError(err) {
					l.Error("grafana-connector: cloud: role change blocked — user is managed by an external identity provider",
						zap.Int("user_id", userID),
						zap.String("current_role", cu.Role),
						zap.String("requested_role", role),
						zap.Strings("auth_labels", cu.AuthLabels),
						zap.String("resolution", "in Grafana, go to Administration → Authentication and enable 'Skip org role sync' for the SSO provider, or set skip_org_role_sync=true via PUT /api/v1/sso-settings/{provider}"),
					)
					return nil, fmt.Errorf("grafana-connector: cloud: user %d role is controlled by an external identity provider (current: %s, requested: %s) — to enable provisioning, set skip_org_role_sync=true in Grafana SSO settings: %w", userID, cu.Role, role, err)
				}
				return nil, fmt.Errorf("grafana-connector: cloud: failed to update role for user %d: %w", userID, err)
			}
			return nil, nil
		}
	}

	// User not in org — retrieve email from UserTrait (set via WithEmail() during List)
	userTrait, err := rs.GetUserTrait(principal)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: cloud: failed to get user trait from principal: %w", err)
	}

	var email string
	for _, e := range userTrait.GetEmails() {
		if e.GetIsPrimary() {
			email = e.GetAddress()
			break
		}
	}
	if email == "" && len(userTrait.GetEmails()) > 0 {
		email = userTrait.GetEmails()[0].GetAddress()
	}
	if email == "" {
		return nil, fmt.Errorf("grafana-connector: cloud: no email found on principal user %d", userID)
	}

	req := &grafana.AddUserToOrgRequest{
		LoginOrEmail: email,
		Role:         role,
	}
	if err = o.client.AddUserToCurrentOrg(ctx, req); err != nil {
		return nil, fmt.Errorf("grafana-connector: cloud: failed to add user %s to org with role %s: %w", email, role, err)
	}

	return nil, nil
}

// grantSelfHosted is the original Grant logic for self-hosted Grafana — unchanged.
func (o *orgBuilder) grantSelfHosted(ctx context.Context, l *zap.Logger, userID, orgID int, role string) (annotations.Annotations, error) {
	l.Debug("Adding user to organization", zap.Int("org_id", orgID), zap.Int("user_id", userID), zap.String("role", role))

	// Find the user in the organization's existing users
	orgsForUser, err := o.client.ListOrgsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to list users in organization %d: %w", orgID, err)
	}

	// Check if user is already in the organization
	for _, orgForUser := range orgsForUser {
		if orgForUser.OrgId == orgID {
			// User already exists in org, check if they have the same role
			if orgForUser.Role == role {
				// User already has the requested role, return GrantAlreadyExists
				return annotations.New(&v2.GrantAlreadyExists{}), nil
			}

			l.Debug("Removing user from organization", zap.Int("org_id", orgID), zap.Int("user_id", userID), zap.String("role", role))
			// User exists but with a different role
			// Remove the user first to update their role
			err = o.client.RemoveUserFromOrg(ctx, strconv.Itoa(orgForUser.OrgId), userID)
			if err != nil {
				return nil, fmt.Errorf("grafana-connector: failed to remove user %d from organization %d to update role: %w", userID, orgID, err)
			}
			break
		}
	}

	grafanaUser, err := o.client.GetUserByID(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to get user by ID %d: %w", userID, err)
	}

	// Create the request to add the user to the organization
	req := &grafana.AddUserToOrgRequest{
		LoginOrEmail: grafanaUser.Login,
		Role:         role,
	}

	// Call the API to add the user to the organization
	err = o.client.AddUserToOrg(ctx, strconv.Itoa(orgID), req)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to add user %s to organization %d with role %s: %w", grafanaUser.Login, orgID, role, err)
	}

	return nil, nil
}

// Revoke removes a user from an organization.
func (o *orgBuilder) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	// Verify the principal is a user
	if grant.Principal.Id.ResourceType != resourceTypeUser.Id {
		return nil, fmt.Errorf("grafana-connector: principal must be a user, got %s", grant.Principal.Id.ResourceType)
	}

	// Get the organization ID
	orgID, err := strconv.Atoi(grant.Entitlement.Resource.Id.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: invalid organization ID %s: %w", grant.Entitlement.Resource.Id.Resource, err)
	}

	// Parse the role from the entitlement ID if needed for debugging
	parts := strings.Split(grant.Entitlement.Id, ":")
	if len(parts) != 3 {
		return nil, fmt.Errorf("grafana-connector: invalid entitlement ID format %s", grant.Entitlement.Id)
	}

	// Get the user ID from the principal
	userID, err := strconv.Atoi(grant.Principal.Id.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: invalid user ID %s: %w", grant.Principal.Id.Resource, err)
	}

	l := ctxzap.Extract(ctx)

	if o.client.IsCloud() {
		return o.revokeCloud(ctx, l, userID, orgID)
	}
	return o.revokeSelfHosted(ctx, l, userID, orgID, grant)
}

// revokeCloud handles Revoke in Cloud mode.
// Strategy:
//  1. List current org users to check membership.
//  2. Not found → GrantAlreadyRevoked (idempotent).
//  3. Found → DELETE /api/org/users/:userId.
func (o *orgBuilder) revokeCloud(ctx context.Context, l *zap.Logger, userID, orgID int) (annotations.Annotations, error) {
	l.Debug("Cloud mode: revoking org membership", zap.Int("user_id", userID), zap.Int("org_id", orgID))

	currentUsers, err := o.client.ListCurrentOrgUsers(ctx)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: cloud: failed to list org users for revoke: %w", err)
	}

	found := false
	var isExternallySynced bool
	for _, cu := range currentUsers {
		if cu.ID == userID {
			found = true
			isExternallySynced = cu.IsExternallySynced
			break
		}
	}

	if !found {
		return annotations.New(&v2.GrantAlreadyRevoked{}), nil
	}

	if err = o.client.RemoveCurrentOrgUser(ctx, userID); err != nil {
		if isExternallySynced && isExternallySyncedRoleError(err) {
			l.Error("grafana-connector: cloud: membership revoke blocked — user is managed by an external identity provider",
				zap.Int("user_id", userID),
				zap.String("resolution", "in Grafana, go to Administration → Authentication and enable 'Skip org role sync' for the SSO provider, or set skip_org_role_sync=true via PUT /api/v1/sso-settings/{provider}"),
			)
			return nil, fmt.Errorf("grafana-connector: cloud: user %d is managed by an external identity provider and cannot be removed via the API — to enable provisioning, set skip_org_role_sync=true in Grafana SSO settings: %w", userID, err)
		}
		return nil, fmt.Errorf("grafana-connector: cloud: failed to remove user %d from org: %w", userID, err)
	}

	return nil, nil
}

// revokeSelfHosted is the original Revoke logic for self-hosted Grafana — unchanged.
func (o *orgBuilder) revokeSelfHosted(ctx context.Context, l *zap.Logger, userID, orgID int, g *v2.Grant) (annotations.Annotations, error) {
	l.Debug("Removing user from organization", zap.Int("org_id", orgID), zap.Int("user_id", userID))

	// Check if the user is in the organization
	orgsForUser, err := o.client.ListOrgsForUser(ctx, userID)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to list users in organization %d: %w", orgID, err)
	}

	userHasOrg := false
	for _, orgForUser := range orgsForUser {
		if orgForUser.OrgId == orgID {
			userHasOrg = true
			break
		}
	}

	// If user is not in the organization, return GrantAlreadyRevoked
	if !userHasOrg {
		return annotations.New(&v2.GrantAlreadyRevoked{}), nil
	}

	// Call the API to remove the user from the organization
	err = o.client.RemoveUserFromOrg(ctx, g.Entitlement.Resource.Id.Resource, userID)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to remove user %d from organization %d: %w",
			userID, orgID, err)
	}

	return nil, nil
}

// isExternallySyncedRoleError reports whether a Grafana API error is the 403
// "cannot change role for externally synced user" response. This error is
// returned when the org role is controlled by an external identity provider
// (SSO/SAML/OAuth) and skip_org_role_sync is not enabled on the Grafana instance.
func isExternallySyncedRoleError(err error) bool {
	return strings.Contains(err.Error(), "cannot change role for externally synced user")
}

func newOrgBuilder(client *grafana.Client) *orgBuilder {
	return &orgBuilder{
		resourceType: resourceTypeOrg,
		client:       client,
	}
}
