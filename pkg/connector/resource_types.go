package connector

import (
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
)

var (
	resourceTypeOrg = &v2.ResourceType{
		Id:          "org",
		DisplayName: "Organization",
	}
	resourceTypeUser = &v2.ResourceType{
		Id:          "user",
		DisplayName: "User",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
		Annotations: annotationsForUserResourceType(),
	}
	resourceTypeTeam = &v2.ResourceType{
		Id:          "team",
		DisplayName: "Team",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_GROUP},
		Annotations: annotations.New(&v2.SkipEntitlements{}),
	}
	resourceTypeRole = &v2.ResourceType{
		Id:          "role",
		DisplayName: "Role",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_ROLE},
		// TypeScopedGrants: team→role assignments are emitted once per sync by
		// roleBuilder.GrantsForResourceType. The SDK only schedules that op when
		// the role type is in the sync, so OptIn-off tenants never mint grants
		// against unsynced role entitlements. StaticEntitlements still provides
		// the assignment entitlement, hence SkipEntitlements (no per-role list).
		Annotations: annotations.New(&v2.SkipEntitlements{}, &v2.TypeScopedGrants{}, &v2.OptInRequired{}),
	}
	resourceTypeServiceAccount = &v2.ResourceType{
		Id:          "service_account",
		DisplayName: "Service Account",
		Traits:      []v2.ResourceType_Trait{v2.ResourceType_TRAIT_USER},
		Annotations: annotations.New(&v2.SkipEntitlements{}),
	}
)
