package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"

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

type teamBuilder struct {
	client *grafana.Client
}

func newTeamBuilder(client *grafana.Client) *teamBuilder {
	return &teamBuilder{client: client}
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

func (t *teamBuilder) List(ctx context.Context, _ *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	bag, page, err := parsePageToken(pToken, &v2.ResourceId{ResourceType: resourceTypeTeam.Id})
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to parse page token: %w", err)
	}

	teams, nextPage, annos, err := t.client.ListTeams(ctx, &grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	})
	if err != nil {
		return nil, "", annos, fmt.Errorf("grafana-connector: failed to list teams: %w", err)
	}

	next, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, "", annos, fmt.Errorf("grafana-connector: failed to generate next page token: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(teams))
	for _, team := range teams {
		if team == nil {
			continue
		}
		resource, err := teamResource(team)
		if err != nil {
			return nil, "", annos, fmt.Errorf("grafana-connector: failed to create team resource %q: %w", team.Name, err)
		}
		resources = append(resources, resource)
	}

	return resources, next, annos, nil
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

// Grants emits team membership only. Team→RBAC-role assignments are emitted by
// roleBuilder.GrantsForResourceType (TypeScopedGrants) so they are tied to the
// role type's own sync lifecycle and never reference unsynced role entitlements
// when the OptIn-required role type is off.
func (t *teamBuilder) Grants(ctx context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	teamID, err := strconv.Atoi(resource.Id.Resource)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: invalid team id %q: %w", resource.Id.Resource, err)
	}

	members, annos, err := t.client.ListTeamMembers(ctx, teamID)
	if err != nil {
		return nil, "", annos, fmt.Errorf("grafana-connector: failed to list members for team %d: %w", teamID, err)
	}

	grants := make([]*v2.Grant, 0, len(members))
	for _, member := range members {
		if member == nil {
			continue
		}
		principalID, err := rs.NewResourceID(resourceTypeUser, member.UserID)
		if err != nil {
			return nil, "", annos, fmt.Errorf("grafana-connector: failed to build user resource id for team member %d: %w", member.UserID, err)
		}
		grants = append(grants, grant.NewGrant(resource, teamMemberEntitlement, principalID))
	}

	return grants, "", annos, nil
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
