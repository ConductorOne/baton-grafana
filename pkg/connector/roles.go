package connector

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	ent "github.com/conductorone/baton-sdk/pkg/types/entitlement"
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

// Grants is a no-op. Grafana lists role assignments per team, so team→role
// grants are emitted from teamBuilder.Grants (the principal that carries them);
// the role type declares SkipGrants.
func (r *roleBuilder) Grants(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}
