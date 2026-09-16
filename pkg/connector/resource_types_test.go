package connector

import (
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
)

func TestResourceTypesDeclareCapabilityPermissions(t *testing.T) {
	resourceTypes := []*v2.ResourceType{
		resourceTypeOrg,
		resourceTypeUser,
		resourceTypeTeam,
		resourceTypeRole,
		resourceTypeServiceAccount,
	}
	for _, resourceType := range resourceTypes {
		t.Run(resourceType.GetId(), func(t *testing.T) {
			perms := &v2.CapabilityPermissions{}
			resourceAnnos := annotations.Annotations(resourceType.GetAnnotations())
			ok, err := resourceAnnos.Pick(perms)
			if err != nil {
				t.Fatalf("unexpected error picking CapabilityPermissions: %v", err)
			}
			if !ok {
				t.Fatalf("resource type %s is missing a CapabilityPermissions annotation", resourceType.GetId())
			}
			if len(perms.GetPermissions()) == 0 {
				t.Fatalf("resource type %s has an empty CapabilityPermissions list", resourceType.GetId())
			}
		})
	}
}

func TestServiceAccountRuntimeResourceTypeCarriesCapabilityPermissions(t *testing.T) {
	for _, syncOrgs := range []bool{true, false} {
		rt := serviceAccountResourceType(syncOrgs)
		perms := &v2.CapabilityPermissions{}
		resourceAnnos := annotations.Annotations(rt.GetAnnotations())
		ok, err := resourceAnnos.Pick(perms)
		if err != nil {
			t.Fatalf("unexpected error picking CapabilityPermissions: %v", err)
		}
		if !ok {
			t.Fatalf("runtime service account resource type (syncOrgs=%v) is missing a CapabilityPermissions annotation", syncOrgs)
		}
		if len(perms.GetPermissions()) == 0 {
			t.Fatalf("runtime service account resource type (syncOrgs=%v) has an empty CapabilityPermissions list", syncOrgs)
		}
	}
}
