package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	ent "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

var _ connectorbuilder.ResourceSyncerV2 = (*teamBuilder)(nil)

type teamBuilder struct {
	client    *grafana.Client
	syncRoles bool // false skips the team→role Grants page
}

func newTeamBuilder(client *grafana.Client, syncRoles bool) *teamBuilder {
	return &teamBuilder{client: client, syncRoles: syncRoles}
}

func (t *teamBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeTeam
}

func teamResource(team *grafana.Team) (*v2.Resource, error) {
	profile := map[string]any{
		profileKeyTeamID:      team.ID,
		profileKeyUID:         team.UID,
		profileKeyOrgID:       strconv.Itoa(team.OrgID),
		profileKeyEmail:       team.Email,
		profileKeyMemberCount: team.MemberCount,
	}

	return rs.NewGroupResource(
		team.Name,
		resourceTypeTeam,
		team.ID,
		nil,
		rs.WithResourceProfile(profile),
	)
}

func (t *teamBuilder) List(ctx context.Context, _ *v2.ResourceId, attrs rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	bag, page, err := parsePageToken(&attrs.PageToken, &v2.ResourceId{ResourceType: resourceTypeTeam.Id})
	if err != nil {
		return nil, nil, fmt.Errorf("grafana-connector: failed to parse page token: %w", err)
	}

	teams, nextPage, annos, err := t.client.ListTeams(ctx, &grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	})
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to list teams: %w", err)
	}

	next, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to generate next page token: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(teams))
	for _, team := range teams {
		if team == nil {
			continue
		}
		resource, err := teamResource(team)
		if err != nil {
			return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to create team resource %q: %w", team.Name, err)
		}
		resources = append(resources, resource)
	}

	return resources, &rs.SyncOpResults{NextPageToken: next, Annotations: annos}, nil
}

// Entitlements is a no-op — every team shares the same membership entitlement
// via StaticEntitlements.
func (t *teamBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// StaticEntitlements declares the uniform team membership entitlement.
// GET /api/teams/{id}/members is the same shape for every team.
func (t *teamBuilder) StaticEntitlements(_ context.Context, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return []*v2.Entitlement{
		ent.NewAssignmentEntitlement(
			nil,
			teamMemberEntitlement,
			ent.WithGrantableTo(resourceTypeUser),
			ent.WithDisplayName("Team Member"),
			ent.WithDescription("Member of a Grafana team"),
		),
	}, nil, nil
}

// Grants emits membership, then team RBAC roles on a second page when roles are in the sync.
func (t *teamBuilder) Grants(ctx context.Context, resource *v2.Resource, attrs rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	teamID, err := strconv.Atoi(resource.Id.Resource)
	if err != nil {
		return nil, nil, fmt.Errorf("grafana-connector: invalid team id %q: %w", resource.Id.Resource, err)
	}

	switch page := attrs.PageToken.Token; page {
	case syncRolesToken:
		roleGrants, annos, err := t.roleGrants(ctx, resource, teamID)
		if err != nil {
			return nil, &rs.SyncOpResults{Annotations: annos}, err
		}
		return roleGrants, &rs.SyncOpResults{Annotations: annos}, nil
	case "":
	default:
		return nil, nil, fmt.Errorf("grafana-connector: unexpected team grants page token %q", page)
	}

	members, annos, err := t.client.ListTeamMembers(ctx, teamID)
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to list members for team %d: %w", teamID, err)
	}

	grants := make([]*v2.Grant, 0, len(members))
	for _, member := range members {
		if member == nil {
			continue
		}
		principalID, err := rs.NewResourceID(resourceTypeUser, member.UserID)
		if err != nil {
			return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to build user resource id for team member %d: %w", member.UserID, err)
		}
		grants = append(grants, grant.NewGrant(resource, teamMemberEntitlement, principalID))
	}

	if !t.syncRoles {
		ctxzap.Extract(ctx).Debug(
			"grafana-connector: role type is not part of this sync; skipping team role grants",
			zap.Int("team_id", teamID),
		)
		return grants, &rs.SyncOpResults{Annotations: annos}, nil
	}

	return grants, &rs.SyncOpResults{NextPageToken: syncRolesToken, Annotations: annos}, nil
}

// roleGrants lists the RBAC roles assigned to this team. A 404 means the
// instance has no access-control API at all, so there is nothing to emit and
// the page skips. Every other failure — including a 403 from a credential
// missing `teams.roles:read` — fails the page: emitting zero grants would tell
// C1 that every team's role assignments were revoked.
func (t *teamBuilder) roleGrants(ctx context.Context, resource *v2.Resource, teamID int) ([]*v2.Grant, annotations.Annotations, error) {
	roles, annos, err := t.client.ListRolesForTeam(ctx, teamID)
	switch {
	case err == nil:
	case errors.Is(err, grafana.ErrRBACUnavailable):
		ctxzap.Extract(ctx).Debug(
			"grafana-connector: access-control api unavailable; skipping team role grants",
			zap.Int("team_id", teamID),
		)
		return nil, annos, nil
	case errors.Is(err, grafana.ErrRBACForbidden):
		return nil, annos, fmt.Errorf("grafana-connector: failed to list roles for team %d: the credential is missing the `teams.roles:read` permission: %w", teamID, err)
	default:
		return nil, annos, fmt.Errorf("grafana-connector: failed to list roles for team %d: %w", teamID, err)
	}

	return roleGrantsForTeam(resource.Id, emitableRoleNames(roles)), annos, nil
}

// emitableRoleNames keeps only the RBAC role names that roleBuilder.List would
// have synced, so team→role grants never target a role resource it filtered out.
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

func parseTeamMembershipIDs(principal *v2.Resource, teamResourceID string) (int, int, error) {
	if principal.Id.ResourceType != resourceTypeUser.Id {
		return 0, 0, status.Errorf(codes.InvalidArgument, "grafana-connector: team membership principal must be a user, got %s", principal.Id.ResourceType)
	}
	teamID, err := strconv.Atoi(teamResourceID)
	if err != nil {
		return 0, 0, status.Errorf(codes.InvalidArgument, "grafana-connector: invalid team id %q: %v", teamResourceID, err)
	}
	userID, err := strconv.Atoi(principal.Id.Resource)
	if err != nil {
		return 0, 0, status.Errorf(codes.InvalidArgument, "grafana-connector: invalid user id %q: %v", principal.Id.Resource, err)
	}
	return teamID, userID, nil
}

func (t *teamBuilder) Grant(ctx context.Context, principal *v2.Resource, entitlement *v2.Entitlement) (annotations.Annotations, error) {
	teamID, userID, err := parseTeamMembershipIDs(principal, entitlement.Resource.Id.Resource)
	if err != nil {
		return nil, err
	}

	annos, err := t.client.AddUserToTeam(ctx, teamID, userID)
	if err != nil {
		if errors.Is(err, grafana.ErrTeamMemberAlreadyExists) {
			annos.Update(&v2.GrantAlreadyExists{})
			return annos, nil
		}
		return annos, fmt.Errorf("grafana-connector: failed to add user %d to team %d: %w", userID, teamID, err)
	}

	return annos, nil
}

func (t *teamBuilder) Revoke(ctx context.Context, grant *v2.Grant) (annotations.Annotations, error) {
	teamID, userID, err := parseTeamMembershipIDs(grant.Principal, grant.Entitlement.Resource.Id.Resource)
	if err != nil {
		return nil, err
	}

	annos, err := t.client.RemoveUserFromTeam(ctx, teamID, userID)
	if err != nil {
		if errors.Is(err, grafana.ErrTeamMemberNotFound) {
			annos.Update(&v2.GrantAlreadyRevoked{})
			return annos, nil
		}
		return annos, fmt.Errorf("grafana-connector: failed to remove user %d from team %d: %w", userID, teamID, err)
	}

	return annos, nil
}
