package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	ent "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type roleBuilder struct {
	client *grafana.Client
}

func newRoleBuilder(client *grafana.Client) *roleBuilder {
	return &roleBuilder{client: client}
}

func (r *roleBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeRole
}

func roleResource(role grafana.Role) (*v2.Resource, error) {
	displayName := role.DisplayName
	if displayName == "" {
		displayName = role.Name
	}
	// IRM and OnCall expose the same displayName for sibling roles ("Admin",
	// "Alert Groups Editor", ...). Suffix the API's group so the two catalogs
	// are distinguishable in the UI. The API reports "IRM" and "Grafana OnCall";
	// trim the redundant "Grafana " so the suffix reads "(OnCall)".
	if group := strings.TrimPrefix(role.Group, "Grafana "); group != "" {
		displayName = fmt.Sprintf("%s (%s)", displayName, group)
	}
	profile := map[string]any{
		profileKeyUID:         role.UID,
		profileKeyName:        role.Name,
		profileKeyDescription: role.Description,
		profileKeyGroup:       role.Group,
		profileKeyGlobal:      role.Global,
	}

	return rs.NewRoleResource(
		displayName,
		resourceTypeRole,
		role.Name,
		nil,
		rs.WithResourceProfile(profile),
	)
}

func (r *roleBuilder) List(ctx context.Context, _ *v2.ResourceId, _ *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	// Access-control is OptInRequired. When the type is scheduled, an empty
	// successful List is authoritative and would wipe previously synced roles,
	// so an absent access-control API must fail the sync instead.
	roles, err := r.client.ListRoles(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list roles: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(roles))
	for _, role := range roles {
		if !shouldEmitRole(role) {
			continue
		}
		resource, err := roleResource(role)
		if err != nil {
			return nil, "", nil, fmt.Errorf("grafana-connector: failed to create role resource %q: %w", role.Name, err)
		}
		resources = append(resources, resource)
	}

	return resources, "", nil, nil
}

// Entitlements is a no-op — every role shares the same assignment entitlement
// via StaticEntitlements.
func (r *roleBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// StaticEntitlements declares the uniform role assignment entitlement.
func (r *roleBuilder) StaticEntitlements(_ context.Context, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return []*v2.Entitlement{
		ent.NewAssignmentEntitlement(
			nil,
			roleAssignedEntitlement,
			ent.WithGrantableTo(resourceTypeTeam),
			ent.WithDisplayName("Assigned"),
			ent.WithDescription("Assignment of a Grafana RBAC role"),
		),
	}, "", nil, nil
}

// Grants is a no-op. The role type carries the TypeScopedGrants annotation, so
// the SDK never schedules a per-resource grants op — team→role assignments are
// emitted once per sync by GrantsForResourceType below.
func (r *roleBuilder) Grants(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// GrantsForResourceType emits every team→RBAC-role assignment for the role type.
// The SDK calls this once per sync (paginating over teams) and only when the
// role type is actually in the sync — so an OptIn-off tenant never mints grants
// against role entitlements that were not synced, and emission is scoped to the
// role type's own sync lifecycle rather than a process-global flag (safe under
// service-mode reuse and checkpoint resume). POST /api/access-control/teams/
// roles/search batches the whole page of teams in one call.
func (r *roleBuilder) GrantsForResourceType(ctx context.Context, _ string, opts rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	pToken := opts.PageToken
	bag, page, err := parsePageToken(&pToken, &v2.ResourceId{ResourceType: resourceTypeTeam.Id})
	if err != nil {
		return nil, nil, fmt.Errorf("grafana-connector: failed to parse page token: %w", err)
	}
	if page == 0 {
		page = 1
	}

	teams, numNextPage, err := r.client.ListTeams(ctx, &grafana.PaginationVars{Size: ResourcesPageSize, Page: page})
	if err != nil {
		return nil, nil, fmt.Errorf("grafana-connector: failed to list teams for role grants: %w", err)
	}

	var pageToken string
	if numNextPage > 0 {
		pageToken = strconv.FormatUint(numNextPage, 10)
	}
	next, err := bag.NextToken(pageToken)
	if err != nil {
		return nil, nil, fmt.Errorf("grafana-connector: failed to generate next page token: %w", err)
	}

	var grants []*v2.Grant
	if len(teams) > 0 {
		teamIDs := make([]int, 0, len(teams))
		for _, team := range teams {
			teamIDs = append(teamIDs, team.ID)
		}

		rolesByTeam, err := r.client.ListRolesForTeams(ctx, teamIDs)
		if err != nil {
			// This method only runs when the role type is opted in, so RBAC must
			// be reachable. A route-absent 404 (or any error) here is not "roles
			// disabled" — fail closed rather than emit an empty set that C1 would
			// read as a revoke of every team→role grant.
			return nil, nil, fmt.Errorf("grafana-connector: failed to list roles for teams: %w", err)
		}

		for _, team := range teams {
			teamID := &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: strconv.Itoa(team.ID)}
			grants = append(grants, roleGrantsForTeam(teamID, emitableRoleNames(rolesByTeam[strconv.Itoa(team.ID)]))...)
		}
	}

	return grants, &rs.SyncOpResults{NextPageToken: next}, nil
}

// emitableRoleNames keeps only the RBAC role names that List would have synced,
// so team→role grants never target a role resource that was filtered out.
func emitableRoleNames(roles []grafana.Role) []string {
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		if shouldEmitRole(role) {
			out = append(out, role.Name)
		}
	}
	return out
}

// roleGrantsForTeam builds team→role grants (role resource bearing the assigned
// entitlement, team as principal) with a GrantExpandable pointing at the team's
// member entitlement so members inherit the role in C1.
func roleGrantsForTeam(teamID *v2.ResourceId, roleNames []string) []*v2.Grant {
	teamResource := &v2.Resource{Id: teamID}
	memberEntitlementID := ent.NewEntitlementID(teamResource, teamMemberEntitlement)
	out := make([]*v2.Grant, 0, len(roleNames))
	for _, name := range roleNames {
		if name == "" || !isIRMOrOnCallRole(name) {
			continue
		}
		roleID := &v2.ResourceId{ResourceType: resourceTypeRole.Id, Resource: name}
		out = append(out, grant.NewGrant(
			&v2.Resource{Id: roleID},
			roleAssignedEntitlement,
			teamResource.Id,
			grant.WithAnnotation(&v2.GrantExpandable{
				EntitlementIds: []string{memberEntitlementID},
				Shallow:        true,
			}),
		))
	}
	return out
}

func (r *roleBuilder) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
	if principal == nil || principal.Id == nil {
		return nil, status.Error(codes.InvalidArgument, "grafana-connector: role grant principal is required")
	}
	if entitlement == nil || entitlement.Resource == nil || entitlement.Resource.Id == nil {
		return nil, status.Error(codes.InvalidArgument, "grafana-connector: role grant entitlement resource is required")
	}
	if principal.Id.ResourceType != resourceTypeTeam.Id {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: rbac role principal must be a team, got %s", principal.Id.ResourceType)
	}

	teamID, err := strconv.Atoi(principal.Id.Resource)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: invalid team id %q: %v", principal.Id.Resource, err)
	}

	roleName := entitlement.Resource.Id.Resource
	if !isIRMOrOnCallRole(roleName) {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: role %q is outside the synced IRM/OnCall role catalog", roleName)
	}

	existing, err := r.client.ListTeamRoles(ctx, teamID)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to list team roles before grant: %w", err)
	}
	for _, role := range existing {
		if role.Name == roleName {
			// POST /api/access-control/teams/{id}/roles returns 200 even when
			// the role is already assigned — detect via pre-read instead.
			return annotations.New(&v2.GrantAlreadyExists{}), nil
		}
	}

	role, err := r.resolveRole(ctx, roleName)
	if err != nil {
		return nil, err
	}
	if !shouldEmitRole(*role) {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: role %q is hidden or outside the synced IRM/OnCall role catalog", roleName)
	}

	if err := r.client.AssignRoleToTeam(ctx, teamID, role.UID); err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to assign role %q to team %d: %w", roleName, teamID, err)
	}

	return nil, nil
}

func (r *roleBuilder) Revoke(ctx context.Context, g *v2.Grant) (annotations.Annotations, error) {
	if g == nil || g.Principal == nil || g.Principal.Id == nil {
		return nil, status.Error(codes.InvalidArgument, "grafana-connector: role revoke principal is required")
	}
	if g.Entitlement == nil || g.Entitlement.Resource == nil || g.Entitlement.Resource.Id == nil {
		return nil, status.Error(codes.InvalidArgument, "grafana-connector: role revoke entitlement resource is required")
	}
	principal := g.Principal
	if principal.Id.ResourceType != resourceTypeTeam.Id {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: rbac role principal must be a team, got %s", principal.Id.ResourceType)
	}

	teamID, err := strconv.Atoi(principal.Id.Resource)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: invalid team id %q: %v", principal.Id.Resource, err)
	}

	roleName := g.Entitlement.Resource.Id.Resource
	if !isIRMOrOnCallRole(roleName) {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: role %q is outside the synced IRM/OnCall role catalog", roleName)
	}

	existing, err := r.client.ListTeamRoles(ctx, teamID)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to list team roles before revoke: %w", err)
	}
	var roleUID string
	for _, role := range existing {
		if role.Name == roleName {
			roleUID = role.UID
			break
		}
	}
	if roleUID == "" {
		return annotations.New(&v2.GrantAlreadyRevoked{}), nil
	}

	if err := r.client.RemoveRoleFromTeam(ctx, teamID, roleUID); err != nil {
		if errors.Is(err, grafana.ErrTeamRoleNotFound) {
			return annotations.New(&v2.GrantAlreadyRevoked{}), nil
		}
		return nil, fmt.Errorf("grafana-connector: failed to remove role %q from team %d: %w", roleName, teamID, err)
	}

	return nil, nil
}

// resolveRole resolves the current role from its stable API name instead of
// trusting an instance-specific UID captured by an earlier sync.
func (r *roleBuilder) resolveRole(ctx context.Context, roleName string) (*grafana.Role, error) {
	roles, err := r.client.ListRoles(ctx)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to resolve role for %q: %w", roleName, err)
	}
	for _, role := range roles {
		if role.Name == roleName {
			return &role, nil
		}
	}
	return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: role %q not found", roleName)
}
