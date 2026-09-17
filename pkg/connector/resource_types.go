package connector

import (
	"slices"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
)

// capabilityPermissions builds the CapabilityPermissions annotation declaring
// the RBAC action names a resource type's sync/provisioning calls require.
//
// These are Grafana Cloud's native RBAC action names. Self-hosted Grafana OSS
// gates the same calls behind a single server-admin flag with no named
// permission strings, and self-hosted Enterprise mixes some of these RBAC
// actions with legacy admin-API-only ones (see docs/connector.mdx) — neither
// self-hosted mode is representable as a flat permission-string list, so this
// annotation documents the Cloud/RBAC-native set only.
func capabilityPermissions(perms ...string) *v2.CapabilityPermissions {
	permissions := make([]*v2.CapabilityPermission, 0, len(perms))
	for _, perm := range perms {
		permissions = append(permissions, v2.CapabilityPermission_builder{Permission: perm}.Build())
	}
	return v2.CapabilityPermissions_builder{Permissions: permissions}.Build()
}

var (
	resourceTypeOrg = &v2.ResourceType{
		Id:          "org",
		DisplayName: "Organization",
		Annotations: annotations.New(
			capabilityPermissions("org.users:read", "org.users:write", "org.users:remove"),
		),
	}
	resourceTypeUser = &v2.ResourceType{
		Id:          "user",
		DisplayName: "User",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
		Annotations: annotations.New(
			&v2.SkipEntitlementsAndGrants{},
			capabilityPermissions("org.users:read", "org.users:add", "org.users:remove"),
		),
	}
	resourceTypeTeam = &v2.ResourceType{
		Id:          "team",
		DisplayName: "Team",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_GROUP},
		Annotations: annotations.New(
			&v2.SkipEntitlements{},
			capabilityPermissions(slices.Concat(teamBaseCapabilityPermissions, []string{"teams.roles:read"})...),
		),
	}
	resourceTypeRole = &v2.ResourceType{
		Id:          "role",
		DisplayName: "Role",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
		Annotations: annotations.New(
			&v2.SkipEntitlementsAndGrants{},
			&v2.OptInRequired{},
			capabilityPermissions("roles:read"),
		),
	}
	resourceTypeServiceAccount = &v2.ResourceType{
		Id:          "service_account",
		DisplayName: "Service Account",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
		Annotations: annotations.New(
			capabilityPermissions("serviceaccounts:read"),
		),
	}
)
