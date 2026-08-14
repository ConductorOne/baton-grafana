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
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type teamBuilder struct {
	client    *grafana.Client
	syncRoles bool
}

func newTeamBuilder(client *grafana.Client, syncRoles bool) *teamBuilder {
	return &teamBuilder{client: client, syncRoles: syncRoles}
}

func (t *teamBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeTeam
}

func teamResource(team grafana.Team, assignedRoleNames *string) (*v2.Resource, error) {
	profile := map[string]any{
		profileKeyTeamID:      team.ID,
		profileKeyUID:         team.UID,
		profileKeyOrgID:       team.OrgID,
		profileFieldEmail:     team.Email,
		profileKeyMemberCount: team.MemberCount,
	}
	if assignedRoleNames != nil {
		profile[profileKeyAssignedRoleNames] = *assignedRoleNames
	}

	return rs.NewGroupResource(
		team.Name,
		resourceTypeTeam,
		team.ID,
		nil,
		rs.WithResourceProfile(profile),
	)
}

func (t *teamBuilder) List(ctx context.Context, _ *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	// GET /api/teams/search uses 1-based page; page 1 is the first page.
	bag, page, err := parsePageToken(pToken, &v2.ResourceId{ResourceType: resourceTypeTeam.Id})
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to parse page token: %w", err)
	}
	if page == 0 {
		page = 1
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	}

	teams, numNextPage, err := t.client.ListTeams(ctx, &paginationOpts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list teams: %w", err)
	}

	var pageToken string
	if numNextPage > 0 {
		pageToken = strconv.FormatUint(numNextPage, 10)
	}

	next, err := bag.NextToken(pageToken)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to generate next page token: %w", err)
	}

	// One POST /api/access-control/teams/roles/search per List page (batch teamIds).
	// Carry filtered role names on each team profile so Grants does not re-search.
	// Skip when the role type is not in this sync (OptInRequired + not enabled) so
	// we neither call RBAC nor emit dangling role grants.
	rolesByTeam := map[string][]grafana.Role{}
	if t.syncRoles {
		teamIDs := make([]int, 0, len(teams))
		for _, team := range teams {
			teamIDs = append(teamIDs, team.ID)
		}

		var err error
		rolesByTeam, err = t.client.ListRolesForTeams(ctx, teamIDs)
		switch {
		case err == nil:
		case errors.Is(err, grafana.ErrRBACUnavailable):
			ctxzap.Extract(ctx).Debug(
				"grafana-connector: access-control unavailable; team List continuing without role assignments",
			)
			rolesByTeam = map[string][]grafana.Role{}
		default:
			return nil, "", nil, fmt.Errorf("grafana-connector: failed to list roles for teams: %w", err)
		}
	}

	resources := make([]*v2.Resource, 0, len(teams))
	for _, team := range teams {
		var assigned *string
		if t.syncRoles {
			joined := strings.Join(emitableRoleNames(rolesByTeam[strconv.Itoa(team.ID)]), ",")
			assigned = &joined
		}
		resource, err := teamResource(team, assigned)
		if err != nil {
			return nil, "", nil, fmt.Errorf("grafana-connector: failed to create team resource %q: %w", team.Name, err)
		}
		resources = append(resources, resource)
	}

	return resources, next, nil, nil
}

func emitableRoleNames(roles []grafana.Role) []string {
	out := make([]string, 0, len(roles))
	for _, role := range roles {
		if shouldEmitRole(role) {
			out = append(out, role.Name)
		}
	}
	return out
}

// Entitlements is a no-op — every team shares the same membership entitlement
// via StaticEntitlements.
func (t *teamBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// StaticEntitlements declares the uniform team membership entitlement.
// GET /api/teams/{id}/members is the same shape for every team.
func (t *teamBuilder) StaticEntitlements(_ context.Context, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return []*v2.Entitlement{
		ent.NewAssignmentEntitlement(
			nil,
			teamMemberEntitlement,
			ent.WithGrantableTo(resourceTypeUser),
			ent.WithDisplayName("Team Member"),
			ent.WithDescription("Member of a Grafana team"),
		),
	}, "", nil, nil
}

func (t *teamBuilder) Grants(ctx context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	teamID, err := strconv.Atoi(resource.Id.Resource)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: invalid team id %q: %w", resource.Id.Resource, err)
	}

	members, err := t.client.ListTeamMembers(ctx, teamID)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list members for team %d: %w", teamID, err)
	}

	grants := make([]*v2.Grant, 0, len(members))
	for _, member := range members {
		principalID, err := rs.NewResourceID(resourceTypeUser, member.UserID)
		if err != nil {
			return nil, "", nil, fmt.Errorf("grafana-connector: failed to build user resource id for team member %d: %w", member.UserID, err)
		}
		grants = append(grants, grant.NewGrant(resource, teamMemberEntitlement, principalID))
	}

	// Team→RBAC role grants. Prefer profile-carried names from List (one search
	// per page). Fallback searches a single team when the key is absent (e.g.
	// tests that call Grants without List). Grant/Revoke keep the per-team GET
	// for live pre-reads.
	//
	// Membership is the primary grant path; listing team roles is secondary. Teams
	// sync on every Grafana edition, so an absent access-control API only skips
	// role grants and keeps the member grants already built. Every other failure
	// (permissions, 5xx) fails closed instead of emitting an empty role set.
	// When the role type is not in this sync, skip entirely to avoid dangling
	// grants to unsynced role entitlements.
	if !t.syncRoles {
		return grants, "", nil, nil
	}

	roleGrants, err := t.teamRoleGrants(ctx, resource, teamID)
	if err != nil {
		if errors.Is(err, grafana.ErrRBACUnavailable) {
			ctxzap.Extract(ctx).Debug(
				"grafana-connector: access-control unavailable; skipping team role grants",
				zap.Int("team_id", teamID),
			)
			return grants, "", nil, nil
		}
		return nil, "", nil, err
	}
	grants = append(grants, roleGrants...)

	return grants, "", nil, nil
}

func (t *teamBuilder) teamRoleGrants(ctx context.Context, teamResource *v2.Resource, teamID int) ([]*v2.Grant, error) {
	if names, ok := assignedRoleNamesFromProfile(teamResource); ok {
		return teamRoleGrantsFromNames(teamResource, names)
	}

	rolesByTeam, err := t.client.ListRolesForTeams(ctx, []int{teamID})
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to list roles for team %d: %w", teamID, err)
	}
	return teamRoleGrantsFromNames(teamResource, emitableRoleNames(rolesByTeam[strconv.Itoa(teamID)]))
}

func assignedRoleNamesFromProfile(resource *v2.Resource) ([]string, bool) {
	raw := resource.GetProfile()
	if raw == nil {
		return nil, false
	}
	joined, ok := rs.GetProfileStringValue(raw, profileKeyAssignedRoleNames)
	if !ok {
		return nil, false
	}
	if joined == "" {
		return nil, true
	}
	return strings.Split(joined, ","), true
}

func teamRoleGrantsFromNames(teamResource *v2.Resource, roleNames []string) ([]*v2.Grant, error) {
	memberEntitlementID := ent.NewEntitlementID(teamResource, teamMemberEntitlement)
	out := make([]*v2.Grant, 0, len(roleNames))
	for _, name := range roleNames {
		if name == "" || !isIRMOrOnCallRole(name) {
			continue
		}
		roleID, err := rs.NewResourceID(resourceTypeRole, name)
		if err != nil {
			return nil, fmt.Errorf("grafana-connector: failed to build role resource id %q: %w", name, err)
		}
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
	return out, nil
}

func (t *teamBuilder) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
	if principal == nil || principal.Id == nil {
		return nil, status.Error(codes.InvalidArgument, "grafana-connector: team membership principal is required")
	}
	if entitlement == nil || entitlement.Resource == nil || entitlement.Resource.Id == nil {
		return nil, status.Error(codes.InvalidArgument, "grafana-connector: team membership entitlement resource is required")
	}
	if principal.Id.ResourceType != resourceTypeUser.Id {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: team membership principal must be a user, got %s", principal.Id.ResourceType)
	}

	teamID, err := strconv.Atoi(entitlement.Resource.Id.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: invalid team id %q: %w", entitlement.Resource.Id.Resource, err)
	}
	userID, err := strconv.Atoi(principal.Id.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: invalid user id %q: %w", principal.Id.Resource, err)
	}

	err = t.client.AddUserToTeam(ctx, teamID, userID)
	if err != nil {
		if errors.Is(err, grafana.ErrTeamMemberAlreadyExists) {
			return annotations.New(&v2.GrantAlreadyExists{}), nil
		}
		return nil, fmt.Errorf("grafana-connector: failed to add user %d to team %d: %w", userID, teamID, err)
	}

	return nil, nil
}

func (t *teamBuilder) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	if grant == nil || grant.Principal == nil || grant.Principal.Id == nil {
		return nil, status.Error(codes.InvalidArgument, "grafana-connector: team membership revoke principal is required")
	}
	if grant.Entitlement == nil || grant.Entitlement.Resource == nil || grant.Entitlement.Resource.Id == nil {
		return nil, status.Error(codes.InvalidArgument, "grafana-connector: team membership revoke entitlement resource is required")
	}
	principal := grant.Principal
	if principal.Id.ResourceType != resourceTypeUser.Id {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: team membership principal must be a user, got %s", principal.Id.ResourceType)
	}

	teamID, err := strconv.Atoi(grant.Entitlement.Resource.Id.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: invalid team id %q: %w", grant.Entitlement.Resource.Id.Resource, err)
	}
	userID, err := strconv.Atoi(principal.Id.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: invalid user id %q: %w", principal.Id.Resource, err)
	}

	err = t.client.RemoveUserFromTeam(ctx, teamID, userID)
	if err != nil {
		if errors.Is(err, grafana.ErrTeamMemberNotFound) {
			return annotations.New(&v2.GrantAlreadyRevoked{}), nil
		}
		return nil, fmt.Errorf("grafana-connector: failed to remove user %d from team %d: %w", userID, teamID, err)
	}

	return nil, nil
}
