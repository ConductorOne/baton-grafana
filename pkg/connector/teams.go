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

func teamResource(team grafana.Team) (*v2.Resource, error) {
	profile := map[string]any{
		profileKeyTeamID:      team.ID,
		profileKeyUID:         team.UID,
		profileKeyOrgID:       team.OrgID,
		profileFieldEmail:     team.Email,
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

	resources := make([]*v2.Resource, 0, len(teams))
	for _, team := range teams {
		resource, err := teamResource(team)
		if err != nil {
			return nil, "", nil, fmt.Errorf("grafana-connector: failed to create team resource %q: %w", team.Name, err)
		}
		resources = append(resources, resource)
	}

	return resources, next, nil, nil
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

	return grants, "", nil, nil
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
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: invalid team id %q: %v", entitlement.Resource.Id.Resource, err)
	}
	userID, err := strconv.Atoi(principal.Id.Resource)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: invalid user id %q: %v", principal.Id.Resource, err)
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
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: invalid team id %q: %v", grant.Entitlement.Resource.Id.Resource, err)
	}
	userID, err := strconv.Atoi(principal.Id.Resource)
	if err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "grafana-connector: invalid user id %q: %v", principal.Id.Resource, err)
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
