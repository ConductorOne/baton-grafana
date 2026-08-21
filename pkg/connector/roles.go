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

func roleResource(role *grafana.Role) (*v2.Resource, error) {
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
	roles, annos, err := r.client.ListRoles(ctx)
	if err != nil {
		if errors.Is(err, grafana.ErrRBACUnavailable) {
			return nil, "", annos, fmt.Errorf("grafana-connector: Grafana access-control API is unavailable; IRM/OnCall roles require Cloud or Enterprise: %w", err)
		}
		return nil, "", annos, fmt.Errorf("grafana-connector: failed to list roles: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(roles))
	for _, role := range roles {
		if !shouldEmitRole(role) {
			continue
		}
		resource, err := roleResource(role)
		if err != nil {
			return nil, "", annos, fmt.Errorf("grafana-connector: failed to create role resource %q: %w", role.Name, err)
		}
		resources = append(resources, resource)
	}

	return resources, "", annos, nil
}

// Entitlements is a no-op — every role shares the same assignment entitlement
// via StaticEntitlements.
func (r *roleBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// StaticEntitlements declares the uniform role assignment entitlement. Roles are
// sync-only: C1 cannot grant to non-user principals, and Grafana RBAC roles are
// assigned to teams, so provisioning is not exposed.
func (r *roleBuilder) StaticEntitlements(_ context.Context, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return []*v2.Entitlement{
		ent.NewAssignmentEntitlement(
			nil,
			roleAssignedEntitlement,
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

	teams, nextPage, annos, err := r.client.ListTeams(ctx, &grafana.PaginationVars{Size: ResourcesPageSize, Page: page})
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to list teams for role grants: %w", err)
	}

	next, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to generate next page token: %w", err)
	}

	var grants []*v2.Grant
	if len(teams) > 0 {
		teamIDs := make([]int, 0, len(teams))
		for _, team := range teams {
			if team == nil {
				continue
			}
			teamIDs = append(teamIDs, team.ID)
		}

		rolesByTeam, roleAnnos, err := r.client.ListRolesForTeams(ctx, teamIDs)
		// Update (don't append) so a single RateLimitDescription survives Pick.
		var roleRL v2.RateLimitDescription
		if ok, pickErr := roleAnnos.Pick(&roleRL); pickErr == nil && ok {
			annos.WithRateLimiting(&roleRL)
		}
		if err != nil {
			// This method only runs when the role type is opted in, so RBAC must
			// be reachable. A route-absent 404 (or any error) here is not "roles
			// disabled" — fail closed rather than emit an empty set that C1 would
			// read as a revoke of every team→role grant.
			if errors.Is(err, grafana.ErrRBACUnavailable) {
				return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: Grafana access-control API is unavailable; IRM/OnCall roles require Cloud or Enterprise: %w", err)
			}
			return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to list roles for teams: %w", err)
		}

		for _, teamID := range teamIDs {
			teamResourceID := &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: strconv.Itoa(teamID)}
			grants = append(grants, roleGrantsForTeam(teamResourceID, emitableRoleNames(rolesByTeam[strconv.Itoa(teamID)]))...)
		}
	}

	return grants, &rs.SyncOpResults{NextPageToken: next, Annotations: annos}, nil
}

// emitableRoleNames keeps only the RBAC role names that List would have synced,
// so team→role grants never target a role resource that was filtered out.
func emitableRoleNames(roles []*grafana.Role) []string {
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
// member entitlement so members inherit the role in C1. Grants are immutable:
// C1 cannot provision non-user principals.
func roleGrantsForTeam(teamID *v2.ResourceId, roleNames []string) []*v2.Grant {
	teamResource := &v2.Resource{Id: teamID}
	memberEntitlementID := ent.NewEntitlementID(teamResource, teamMemberEntitlement)
	out := make([]*v2.Grant, 0, len(roleNames))
	for _, name := range roleNames {
		roleID := &v2.ResourceId{ResourceType: resourceTypeRole.Id, Resource: name}
		out = append(out, grant.NewGrant(
			&v2.Resource{Id: roleID},
			roleAssignedEntitlement,
			teamResource.Id,
			grant.WithAnnotation(
				&v2.GrantExpandable{
					EntitlementIds: []string{memberEntitlementID},
					Shallow:        true,
				},
				&v2.GrantImmutable{},
			),
		))
	}
	return out
}
