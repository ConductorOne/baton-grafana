package connector

import (
	"slices"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
)

func permissionStrings(rt *v2.ResourceType) ([]string, error) {
	perms := &v2.CapabilityPermissions{}
	resourceAnnos := annotations.Annotations(rt.GetAnnotations())
	ok, err := resourceAnnos.Pick(perms)
	if err != nil {
		return nil, err
	}
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(perms.GetPermissions()))
	for _, perm := range perms.GetPermissions() {
		out = append(out, perm.GetPermission())
	}
	slices.Sort(out)
	return out, nil
}

func TestResourceTypesDeclareCapabilityPermissions(t *testing.T) {
	expected := map[string][]string{
		"org":             {"org.users:read", "org.users:remove", "org.users:write"},
		"user":            {"org.users:add", "org.users:read", "org.users:remove"},
		"team":            {"teams.permissions:read", "teams.permissions:write", "teams.roles:read", "teams:read"},
		"role":            {"roles:read"},
		"service_account": {"serviceaccounts:read"},
	}
	resourceTypes := []*v2.ResourceType{
		resourceTypeOrg,
		resourceTypeUser,
		resourceTypeTeam,
		resourceTypeRole,
		resourceTypeServiceAccount,
	}
	for _, resourceType := range resourceTypes {
		t.Run(resourceType.GetId(), func(t *testing.T) {
			perms, err := permissionStrings(resourceType)
			if err != nil {
				t.Fatalf("unexpected error picking CapabilityPermissions: %v", err)
			}
			if perms == nil {
				t.Fatalf("resource type %s is missing a CapabilityPermissions annotation", resourceType.GetId())
			}
			want, ok := expected[resourceType.GetId()]
			if !ok {
				t.Fatalf("no expected permission set defined for resource type %s", resourceType.GetId())
			}
			if !slices.Equal(perms, want) {
				t.Fatalf("resource type %s CapabilityPermissions = %v, want %v", resourceType.GetId(), perms, want)
			}
		})
	}
}

func TestServiceAccountRuntimeResourceTypeCarriesCapabilityPermissions(t *testing.T) {
	want := []string{"serviceaccounts:read"}
	for _, syncOrgs := range []bool{true, false} {
		rt := serviceAccountResourceType(syncOrgs)
		perms, err := permissionStrings(rt)
		if err != nil {
			t.Fatalf("unexpected error picking CapabilityPermissions: %v", err)
		}
		if perms == nil {
			t.Fatalf("runtime service account resource type (syncOrgs=%v) is missing a CapabilityPermissions annotation", syncOrgs)
		}
		if !slices.Equal(perms, want) {
			t.Fatalf("runtime service account resource type (syncOrgs=%v) CapabilityPermissions = %v, want %v", syncOrgs, perms, want)
		}
	}
}

func TestTeamRuntimeResourceTypeCarriesCapabilityPermissions(t *testing.T) {
	tests := []struct {
		syncRoles bool
		want      []string
	}{
		{
			syncRoles: true,
			want:      []string{"teams.permissions:read", "teams.permissions:write", "teams.roles:read", "teams:read"},
		},
		{
			syncRoles: false,
			want:      []string{"teams.permissions:read", "teams.permissions:write", "teams:read"},
		},
	}
	for _, tt := range tests {
		rt := teamResourceType(tt.syncRoles)
		perms, err := permissionStrings(rt)
		if err != nil {
			t.Fatalf("unexpected error picking CapabilityPermissions: %v", err)
		}
		if perms == nil {
			t.Fatalf("runtime team resource type (syncRoles=%v) is missing a CapabilityPermissions annotation", tt.syncRoles)
		}
		if !slices.Equal(perms, tt.want) {
			t.Fatalf("runtime team resource type (syncRoles=%v) CapabilityPermissions = %v, want %v", tt.syncRoles, perms, tt.want)
		}
	}
}
