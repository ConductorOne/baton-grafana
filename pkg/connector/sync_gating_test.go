package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"slices"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/cli"
	"google.golang.org/protobuf/proto"
)

func TestServiceAccountGrantsSkippedWhenOrgNotSynced(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/serviceaccounts/search" {
			writeJSON(w, http.StatusOK, map[string]any{
				"totalCount": 1,
				"page":       1,
				"perPage":    ResourcesPageSize,
				"serviceAccounts": []map[string]any{
					{"id": 20, "uid": "sa-uid", "name": "baton-test", "login": "sa-1-baton-test", "orgId": 1, "role": "Admin"},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	builder := newServiceAccountBuilder(newCloudClientForTest(t, ts), false)

	resourceType := builder.ResourceType(context.Background())
	typeAnnos := annotations.Annotations(resourceType.GetAnnotations())
	if !typeAnnos.Contains(&v2.SkipEntitlementsAndGrants{}) {
		t.Fatal("without the org type in scope the service account type must carry SkipEntitlementsAndGrants")
	}
	if !typeAnnos.Contains(&v2.SkipEntitlements{}) {
		t.Fatal("the org-excluded variant must preserve the base service account annotations")
	}
	if resourceType.GetId() != resourceTypeServiceAccount.GetId() ||
		resourceType.GetDisplayName() != resourceTypeServiceAccount.GetDisplayName() ||
		!slices.Equal(resourceType.GetTraits(), resourceTypeServiceAccount.GetTraits()) {
		t.Fatalf("the variant must differ from the base type only in its annotations, got %+v", resourceType)
	}
	baseAnnos := annotations.Annotations(resourceTypeServiceAccount.GetAnnotations())
	if baseAnnos.Contains(&v2.SkipEntitlementsAndGrants{}) {
		t.Fatal("deriving the variant must not mutate the package-level service account type")
	}

	resources, _, err := builder.List(context.Background(), nil, syncAttrs(""))
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("service accounts must still sync when orgs are out of scope, got %d resources", len(resources))
	}

	grants, _, err := builder.Grants(context.Background(), resources[0], syncAttrs(""))
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected no org role grants, got %d", len(grants))
	}
}

func TestServiceAccountResourceTypeWhenOrgSynced(t *testing.T) {
	builder := newServiceAccountBuilder(nil, true)

	resourceType := builder.ResourceType(context.Background())
	if !proto.Equal(resourceType, resourceTypeServiceAccount) {
		t.Fatalf("expected the default service account type, got %+v", resourceType)
	}
}

func TestTeamRoleGrantsPageNotScheduledWhenRoleNotSynced(t *testing.T) {
	var teamRoleCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/teams/7/members":
			writeJSON(w, http.StatusOK, []map[string]any{
				{"orgId": 1, "teamId": 7, "userId": 14, "email": "a@ex.com", "login": "alice", "name": "Alice", "permission": 0},
			})
		case "/api/access-control/teams/7/roles":
			teamRoleCalls++
			writeJSON(w, http.StatusOK, []map[string]any{
				{"uid": "vis", "name": "plugins:grafana-irm-app:schedules-editor", "displayName": "Schedules Editor"},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	resource := &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: "7"}}
	builder := newTeamBuilder(newCloudClientForTest(t, ts), false)

	members, results, err := builder.Grants(context.Background(), resource, syncAttrs(""))
	if err != nil {
		t.Fatalf("Grants members: %v", err)
	}
	if len(members) != 1 || members[0].Principal.Id.Resource != "14" {
		t.Fatalf("membership must still be emitted, got %+v", members)
	}
	if next := nextPageToken(results); next != "" {
		t.Fatalf("expected no next page when roles are out of scope, got %q", next)
	}
	if teamRoleCalls != 0 {
		t.Fatalf("team-roles endpoint must not be called, got %d calls", teamRoleCalls)
	}
}

func TestWillSyncResourceType(t *testing.T) {
	cases := []struct {
		name          string
		connectorOpts *cli.ConnectorOpts
		wantSyncOrg   bool
		wantSyncRole  bool
		wantSyncUser  bool
	}{
		{
			name:         "no options at all",
			wantSyncOrg:  true,
			wantSyncRole: true,
			wantSyncUser: true,
		},
		{
			name:          "empty selection means everything",
			connectorOpts: &cli.ConnectorOpts{},
			wantSyncOrg:   true,
			wantSyncRole:  true,
			wantSyncUser:  true,
		},
		{
			name: "explicit selection excludes org and role",
			connectorOpts: &cli.ConnectorOpts{
				SyncResourceTypeIDs: []string{resourceTypeUser.Id, resourceTypeTeam.Id, resourceTypeServiceAccount.Id},
			},
			wantSyncOrg:  false,
			wantSyncRole: false,
			wantSyncUser: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			g := &Grafana{connectorOpts: tc.connectorOpts}

			if got := g.willSyncResourceType(resourceTypeOrg.Id); got != tc.wantSyncOrg {
				t.Errorf("org: got %v, want %v", got, tc.wantSyncOrg)
			}
			if got := g.willSyncResourceType(resourceTypeRole.Id); got != tc.wantSyncRole {
				t.Errorf("role: got %v, want %v", got, tc.wantSyncRole)
			}
			if got := g.willSyncResourceType(resourceTypeUser.Id); got != tc.wantSyncUser {
				t.Errorf("user: got %v, want %v", got, tc.wantSyncUser)
			}
		})
	}
}

func TestResourceSyncersAppliesSyncSelection(t *testing.T) {
	ctx := context.Background()
	g := &Grafana{
		connectorOpts: &cli.ConnectorOpts{
			SyncResourceTypeIDs: []string{resourceTypeUser.Id, resourceTypeTeam.Id, resourceTypeServiceAccount.Id},
		},
	}

	var found bool
	for _, syncer := range g.ResourceSyncers(ctx) {
		resourceType := syncer.ResourceType(ctx)
		if resourceType.GetId() != resourceTypeServiceAccount.Id {
			continue
		}
		found = true
		typeAnnos := annotations.Annotations(resourceType.GetAnnotations())
		if !typeAnnos.Contains(&v2.SkipEntitlementsAndGrants{}) {
			t.Fatal("service account syncer must skip grants when the org type is out of scope")
		}
	}
	if !found {
		t.Fatal("service account syncer missing from ResourceSyncers")
	}
}
