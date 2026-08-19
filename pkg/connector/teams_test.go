package connector

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	ent "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
)

func TestTeamListAndMemberGrants(t *testing.T) {
	var roleSearchCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/teams/search":
			writeJSON(w, http.StatusOK, map[string]any{
				"totalCount": 1,
				"page":       1,
				"perPage":    ResourcesPageSize,
				"teams": []map[string]any{
					{"id": 7, "uid": "teamuid", "orgId": 1, "name": "OnCall", "email": "", "memberCount": 1},
				},
			})
		case r.Method == http.MethodGet && r.URL.Path == "/api/teams/7/members":
			writeJSON(w, http.StatusOK, []map[string]any{
				{"orgId": 1, "teamId": 7, "userId": 14, "email": "a@ex.com", "login": "alice", "name": "Alice", "permission": 0},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/access-control/teams/roles/search":
			roleSearchCalls++
			http.NotFound(w, r)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	builder := newTeamBuilder(newCloudClientForTest(t, ts))

	resources, next, _, err := builder.List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if next != "" {
		t.Fatalf("expected empty next token, got %q", next)
	}
	if len(resources) != 1 || resources[0].Id.Resource != "7" {
		t.Fatalf("unexpected resources: %+v", resources)
	}
	if orgID, ok := rs.GetProfileStringValue(resources[0].GetProfile(), profileKeyOrgID); !ok || orgID != "1" {
		t.Fatalf("team org_id must be a string profile value, got ok=%v value=%q", ok, orgID)
	}

	grants, _, _, err := builder.Grants(context.Background(), resources[0], &pagination.Token{})
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	// Team Grants emits membership only; team→role assignments come from
	// roleBuilder.GrantsForResourceType, so the team path never calls RBAC.
	if len(grants) != 1 {
		t.Fatalf("expected only the member grant, got %d", len(grants))
	}
	if grants[0].Entitlement.Resource.Id.ResourceType != resourceTypeTeam.Id || grants[0].Principal.Id.Resource != "14" {
		t.Fatalf("unexpected member grant: %+v", grants[0])
	}
	if roleSearchCalls != 0 {
		t.Fatalf("team List/Grants must not call the RBAC search, got %d calls", roleSearchCalls)
	}
}

func TestTeamAndServiceAccountPagination(t *testing.T) {
	// Same "full page → keep going" rule ListUsers/ListOrgs already use: a page
	// with len == pageSize yields page+1; a short page ends the loop.
	pageSize := int(ResourcesPageSize)

	tests := []struct {
		name string
		path string
		run  func(*testing.T, *httptest.Server)
	}{
		{
			name: "teams",
			path: "/api/teams/search",
			run: func(t *testing.T, ts *httptest.Server) {
				builder := newTeamBuilder(newCloudClientForTest(t, ts))
				token := &pagination.Token{}
				var ids []string
				for {
					resources, next, _, err := builder.List(context.Background(), nil, token)
					if err != nil {
						t.Fatalf("List: %v", err)
					}
					for _, resource := range resources {
						ids = append(ids, resource.Id.Resource)
					}
					if next == "" {
						break
					}
					token = &pagination.Token{Token: next}
				}
				if len(ids) != pageSize+1 {
					t.Fatalf("got %d ids, want %d", len(ids), pageSize+1)
				}
				if ids[0] != "1" || ids[len(ids)-1] != "2" {
					t.Fatalf("first=%s last=%s", ids[0], ids[len(ids)-1])
				}
			},
		},
		{
			name: "service accounts",
			path: "/api/serviceaccounts/search",
			run: func(t *testing.T, ts *httptest.Server) {
				builder := newServiceAccountBuilder(newCloudClientForTest(t, ts), true)
				token := &pagination.Token{}
				var ids []string
				for {
					resources, next, _, err := builder.List(context.Background(), nil, token)
					if err != nil {
						t.Fatalf("List: %v", err)
					}
					for _, resource := range resources {
						ids = append(ids, resource.Id.Resource)
					}
					if next == "" {
						break
					}
					token = &pagination.Token{Token: next}
				}
				if len(ids) != pageSize+1 {
					t.Fatalf("got %d ids, want %d", len(ids), pageSize+1)
				}
				if ids[0] != "1" || ids[len(ids)-1] != "2" {
					t.Fatalf("first=%s last=%s", ids[0], ids[len(ids)-1])
				}
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var pages []string
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodPost && r.URL.Path == "/api/access-control/teams/roles/search" {
					// Pagination fixture focuses on team search paging; RBAC may be absent (OSS).
					writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not found"})
					return
				}
				if r.URL.Path != tt.path {
					http.NotFound(w, r)
					return
				}
				page := r.URL.Query().Get("page")
				pages = append(pages, page)

				n := pageSize
				id := 1
				pageNum := 1
				if page == "2" {
					n = 1
					id = 2
					pageNum = 2
				}
				items := make([]map[string]any, 0, n)
				for i := 0; i < n; i++ {
					itemID := id
					if page != "2" && i > 0 {
						itemID = 1000 + i
					}
					if tt.name == "teams" {
						items = append(items, map[string]any{
							"id": itemID, "uid": "team-" + strconv.Itoa(itemID), "orgId": 1, "name": "Team",
						})
					} else {
						items = append(items, map[string]any{
							"id": itemID, "uid": "sa-" + strconv.Itoa(itemID), "name": "SA", "orgId": 1,
						})
					}
				}
				if tt.name == "teams" {
					writeJSON(w, http.StatusOK, map[string]any{
						"page": pageNum, "perPage": pageSize, "teams": items,
					})
					return
				}
				writeJSON(w, http.StatusOK, map[string]any{
					"page": pageNum, "perPage": pageSize, "serviceAccounts": items,
				})
			}))
			defer ts.Close()

			tt.run(t, ts)
			if strings.Join(pages, ",") != "1,2" {
				t.Fatalf("pages=%v", pages)
			}
		})
	}
}
func TestTeamGrantIdempotent(t *testing.T) {
	var addCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPost && r.URL.Path == "/api/teams/7/members" {
			addCalls++
			writeJSON(w, http.StatusBadRequest, map[string]string{"message": "User is already added to this team"})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	annos, err := newTeamBuilder(newCloudClientForTest(t, ts)).Grant(
		context.Background(),
		&v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeUser.Id, Resource: "14"}},
		&v2.Entitlement{
			Id:       "team:7:member",
			Resource: &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: "7"}},
		},
	)
	if err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if addCalls != 1 {
		t.Fatalf("addCalls=%d", addCalls)
	}
	if !annos.Contains(&v2.GrantAlreadyExists{}) {
		t.Fatal("expected GrantAlreadyExists")
	}
}

func TestTeamGrantAndRevokeSuccess(t *testing.T) {
	var posts, deletes int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/teams/7/members":
			posts++
			writeJSON(w, http.StatusOK, map[string]string{"message": "Member added"})
		case r.Method == http.MethodDelete && r.URL.Path == "/api/teams/7/members/14":
			deletes++
			writeJSON(w, http.StatusOK, map[string]string{"message": "Team member removed"})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	builder := newTeamBuilder(newCloudClientForTest(t, ts))
	principal := &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeUser.Id, Resource: "14"}}
	entitlement := &v2.Entitlement{
		Resource: &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: "7"}},
	}
	if _, err := builder.Grant(context.Background(), principal, entitlement); err != nil {
		t.Fatalf("Grant: %v", err)
	}
	if _, err := builder.Revoke(context.Background(), &v2.Grant{
		Principal:   principal,
		Entitlement: entitlement,
	}); err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if posts != 1 || deletes != 1 {
		t.Fatalf("posts=%d deletes=%d", posts, deletes)
	}
}

func TestTeamRevokeIdempotent(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/teams/7/members/14" {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Team member not found"})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	annos, err := newTeamBuilder(newCloudClientForTest(t, ts)).Revoke(context.Background(), &v2.Grant{
		Principal: &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeUser.Id, Resource: "14"}},
		Entitlement: &v2.Entitlement{
			Resource: &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: "7"}},
		},
	})
	if err != nil {
		t.Fatalf("Revoke: %v", err)
	}
	if !annos.Contains(&v2.GrantAlreadyRevoked{}) {
		t.Fatal("expected GrantAlreadyRevoked")
	}
}

func TestTeamRevokeDoesNotHideMissingTeam(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete && r.URL.Path == "/api/teams/7/members/14" {
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "Team not found"})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	annos, err := newTeamBuilder(newCloudClientForTest(t, ts)).Revoke(context.Background(), &v2.Grant{
		Principal: &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeUser.Id, Resource: "14"}},
		Entitlement: &v2.Entitlement{
			Resource: &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: "7"}},
		},
	})
	if err == nil {
		t.Fatal("expected missing team to remain an error")
	}
	if annos.Contains(&v2.GrantAlreadyRevoked{}) {
		t.Fatal("missing team must not be reported as GrantAlreadyRevoked")
	}
}

func TestRoleListFiltersIRM(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/access-control/roles":
			writeJSON(w, http.StatusOK, []map[string]any{
				{"uid": "a", "name": "plugins:grafana-irm-app:schedules-editor", "displayName": "Schedules Editor", "group": "IRM"},
				{"uid": "b", "name": "fixed:reports:reader", "displayName": "Report reader", "group": "Reports"},
				{"uid": "c", "name": "plugins:grafana-oncall-app:schedules-editor", "displayName": "Schedules Editor", "group": "Grafana OnCall"},
				{"uid": "d", "name": "plugins:grafana-irm-app:admin", "displayName": "Admin", "group": "IRM", "hidden": true},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	resources, _, _, err := newRoleBuilder(newCloudClientForTest(t, ts)).List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resources) != 2 {
		body, _ := json.Marshal(resources)
		t.Fatalf("expected 2 IRM/OnCall roles, got %d: %s", len(resources), body)
	}
	// IRM and OnCall share displayName per role; the API group must disambiguate
	// (with the redundant "Grafana " prefix trimmed from "Grafana OnCall").
	wantNames := map[string]string{
		"plugins:grafana-irm-app:schedules-editor":    "Schedules Editor (IRM)",
		"plugins:grafana-oncall-app:schedules-editor": "Schedules Editor (OnCall)",
	}
	for _, res := range resources {
		want := wantNames[res.Id.Resource]
		if res.DisplayName != want {
			t.Fatalf("role %s: displayName=%q want %q", res.Id.Resource, res.DisplayName, want)
		}
	}
}

func TestRoleStaticEntitlements(t *testing.T) {
	builder := newRoleBuilder(nil)

	dynamic, _, _, err := builder.Entitlements(context.Background(), &v2.Resource{}, &pagination.Token{})
	if err != nil {
		t.Fatalf("Entitlements: %v", err)
	}
	if len(dynamic) != 0 {
		t.Fatalf("expected dynamic Entitlements to be empty, got %d", len(dynamic))
	}

	static, _, _, err := builder.StaticEntitlements(context.Background(), &pagination.Token{})
	if err != nil {
		t.Fatalf("StaticEntitlements: %v", err)
	}
	if len(static) != 1 {
		t.Fatalf("expected 1 static entitlement, got %d", len(static))
	}
	if static[0].Resource != nil {
		t.Fatal("static entitlement must use a nil resource")
	}
	if static[0].Slug != roleAssignedEntitlement {
		t.Fatalf("slug=%q want %q", static[0].Slug, roleAssignedEntitlement)
	}
	if len(static[0].GrantableTo) != 0 {
		t.Fatalf("role assignments are sync-only; grantable_to must be empty, got %v", static[0].GrantableTo)
	}

	roleTypeAnnos := annotations.Annotations(resourceTypeRole.Annotations)
	if !roleTypeAnnos.Contains(&v2.SkipEntitlements{}) {
		t.Fatal("role resource type must carry SkipEntitlements with StaticEntitlements")
	}
	if !roleTypeAnnos.Contains(&v2.TypeScopedGrants{}) {
		t.Fatal("role resource type must carry TypeScopedGrants: team→role grants come from GrantsForResourceType")
	}
	if roleTypeAnnos.Contains(&v2.SkipGrants{}) {
		t.Fatal("role resource type must not carry SkipGrants — it would suppress the type-scoped grants op")
	}
	if !roleTypeAnnos.Contains(&v2.OptInRequired{}) {
		t.Fatal("role resource type must be OptInRequired (access-control is Cloud/Enterprise only)")
	}
}

func TestTeamStaticEntitlements(t *testing.T) {
	builder := newTeamBuilder(nil)

	dynamic, _, _, err := builder.Entitlements(context.Background(), &v2.Resource{}, &pagination.Token{})
	if err != nil {
		t.Fatalf("Entitlements: %v", err)
	}
	if len(dynamic) != 0 {
		t.Fatalf("expected dynamic Entitlements to be empty, got %d", len(dynamic))
	}

	static, _, _, err := builder.StaticEntitlements(context.Background(), &pagination.Token{})
	if err != nil {
		t.Fatalf("StaticEntitlements: %v", err)
	}
	if len(static) != 1 {
		t.Fatalf("expected 1 static entitlement, got %d", len(static))
	}
	if static[0].Resource != nil || static[0].Slug != teamMemberEntitlement {
		t.Fatalf("unexpected static team entitlement: %+v", static[0])
	}
	if len(static[0].GrantableTo) != 1 || static[0].GrantableTo[0].Id != resourceTypeUser.Id {
		t.Fatalf("grantable_to=%v", static[0].GrantableTo)
	}
	teamTypeAnnos := annotations.Annotations(resourceTypeTeam.Annotations)
	if !teamTypeAnnos.Contains(&v2.SkipEntitlements{}) {
		t.Fatal("team resource type must carry SkipEntitlements with StaticEntitlements")
	}
	if teamTypeAnnos.Contains(&v2.OptInRequired{}) {
		t.Fatal("team resource type must sync by default; OptInRequired is only for RBAC roles")
	}
}

func TestServiceAccountListAndOrgGrant(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/serviceaccounts/search" {
			writeJSON(w, http.StatusOK, map[string]any{
				"totalCount": 1,
				"page":       1,
				"perPage":    ResourcesPageSize,
				"serviceAccounts": []map[string]any{
					{
						"id": 20, "uid": "sa-uid", "name": "baton-test", "login": "sa-1-baton-test",
						"orgId": 1, "isDisabled": true, "role": "Admin", "tokens": 1,
					},
				},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	builder := newServiceAccountBuilder(newCloudClientForTest(t, ts), true)
	resources, _, _, err := builder.List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("resources=%d", len(resources))
	}
	if resources[0].Id.Resource != strconv.Itoa(20) {
		t.Fatalf("id=%s", resources[0].Id.Resource)
	}
	status := rs.GetStatus(resources[0])
	if status == nil || status.GetStatus() != v2.Status_RESOURCE_STATUS_DISABLED {
		t.Fatalf("expected disabled service account status, got %v", status)
	}

	grants, _, _, err := builder.Grants(context.Background(), resources[0], &pagination.Token{})
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if len(grants) != 1 {
		t.Fatalf("grants=%d", len(grants))
	}
	grantAnnos := annotations.Annotations(grants[0].Annotations)
	if !grantAnnos.Contains(&v2.GrantImmutable{}) {
		t.Fatal("service account org grants must be GrantImmutable (sync-only; org Grant/Revoke are user-only)")
	}
	if grants[0].Entitlement.Resource.Id.ResourceType != resourceTypeOrg.Id {
		t.Fatalf("entitlement resource type=%s", grants[0].Entitlement.Resource.Id.ResourceType)
	}
	if grants[0].Principal.Id.ResourceType != resourceTypeServiceAccount.Id {
		t.Fatalf("principal type=%s", grants[0].Principal.Id.ResourceType)
	}
	ut, err := rs.GetUserTrait(resources[0])
	if err != nil {
		t.Fatalf("GetUserTrait: %v", err)
	}
	if ut.GetAccountType() != v2.UserTrait_ACCOUNT_TYPE_SERVICE {
		t.Fatalf("account type=%v", ut.GetAccountType())
	}
	nhi, err := rs.GetNonHumanIdentityTrait(resources[0])
	if err != nil {
		t.Fatalf("GetNonHumanIdentityTrait: %v", err)
	}
	if nhi.GetNhiType() != v2.NonHumanIdentityTrait_NHI_TYPE_APP_REGISTRATION {
		t.Fatalf("nhi type=%v", nhi.GetNhiType())
	}
	if nhi.GetNhiDetail() != "grafana.service_account" {
		t.Fatalf("nhi detail=%q", nhi.GetNhiDetail())
	}

	saTypeAnnos := annotations.Annotations(resourceTypeServiceAccount.Annotations)
	if saTypeAnnos.Contains(&v2.OptInRequired{}) {
		t.Fatal("service_account resource type must sync by default; OptInRequired is only for RBAC roles")
	}
}

func TestServiceAccountSkipsOrgGrantsWhenOrgNotSynced(t *testing.T) {
	builder := newServiceAccountBuilder(nil, false)

	rt := builder.ResourceType(context.Background())
	annos := annotations.Annotations(rt.Annotations)
	if !annos.Contains(&v2.SkipEntitlementsAndGrants{}) {
		t.Fatal("when org is excluded from sync, SA type must SkipEntitlementsAndGrants")
	}

	grants, _, _, err := builder.Grants(context.Background(), &v2.Resource{
		Id: &v2.ResourceId{ResourceType: resourceTypeServiceAccount.Id, Resource: "1"},
	}, &pagination.Token{})
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if len(grants) != 0 {
		t.Fatalf("expected no SA→org grants when org is not synced, got %d", len(grants))
	}
}

func TestIsSyncedRBACRole(t *testing.T) {
	cases := []struct {
		name string
		want bool
	}{
		{"plugins:grafana-irm-app:schedules-editor", true},
		{"plugins:grafana-oncall-app:schedules-reader", true},
		{"fixed:reports:reader", false},
		{"extsvc:grafana-irm-app:permissions", false},
	}
	for _, tc := range cases {
		if got := isIRMOrOnCallRole(tc.name); got != tc.want {
			t.Fatalf("%s: got %v want %v", tc.name, got, tc.want)
		}
	}
}

// An opted-in role type must fail the sync when the RBAC API is absent (OSS);
// an empty successful List would wipe the roles C1 already has.
func TestRoleListErrorsWhenRBACUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusNotFound, map[string]string{"message": "Not found"})
	}))
	defer ts.Close()

	_, _, _, err := newRoleBuilder(newCloudClientForTest(t, ts)).List(context.Background(), nil, &pagination.Token{})
	if err == nil {
		t.Fatal("expected List to error when access-control is unavailable")
	}
}

func TestRoleListErrorsWhenRBACForbidden(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusForbidden, map[string]string{"message": "Access denied"})
	}))
	defer ts.Close()

	_, _, _, err := newRoleBuilder(newCloudClientForTest(t, ts)).List(context.Background(), nil, &pagination.Token{})
	if err == nil {
		t.Fatal("expected List to error when access-control is forbidden")
	}
}

// GrantsForResourceType emits team→role grants (role-side, type-scoped), skips
// Hidden and non-IRM/OnCall roles, and batches the whole page in one search.
func TestRoleGrantsForResourceTypeEmitsTeamRoleGrants(t *testing.T) {
	var teamRoleSearchCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/api/teams/search":
			writeJSON(w, http.StatusOK, map[string]any{
				"totalCount": 2,
				"page":       1,
				"perPage":    ResourcesPageSize,
				"teams": []map[string]any{
					{"id": 7, "uid": "t7", "orgId": 1, "name": "A", "memberCount": 0},
					{"id": 8, "uid": "t8", "orgId": 1, "name": "B", "memberCount": 0},
				},
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/access-control/teams/roles/search":
			teamRoleSearchCalls++
			var req struct {
				TeamIDs []int `json:"teamIds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode search body: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			out := map[string]any{}
			for _, id := range req.TeamIDs {
				key := strconv.Itoa(id)
				if id == 7 {
					out[key] = []map[string]any{
						{"uid": "vis", "name": "plugins:grafana-irm-app:schedules-editor", "displayName": "Schedules Editor"},
						{"uid": "hid", "name": "plugins:grafana-irm-app:admin", "displayName": "Admin", "hidden": true},
						{"uid": "fix", "name": "fixed:reports:reader", "displayName": "Report reader"},
					}
				} else {
					out[key] = []map[string]any{}
				}
			}
			writeJSON(w, http.StatusOK, out)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	builder := newRoleBuilder(newCloudClientForTest(t, ts))
	grants, res, err := builder.GrantsForResourceType(context.Background(), resourceTypeRole.Id, rs.SyncOpAttrs{})
	if err != nil {
		t.Fatalf("GrantsForResourceType: %v", err)
	}
	if res == nil || res.NextPageToken != "" {
		t.Fatalf("expected single page, got results=%+v", res)
	}
	if teamRoleSearchCalls != 1 {
		t.Fatalf("expected one batched team-roles search call, got %d", teamRoleSearchCalls)
	}
	// Only team 7's visible IRM role survives (Hidden + fixed: are filtered).
	if len(grants) != 1 {
		t.Fatalf("expected 1 emitted role grant, got %d", len(grants))
	}
	g := grants[0]
	if g.Entitlement.Resource.Id.ResourceType != resourceTypeRole.Id ||
		g.Entitlement.Resource.Id.Resource != "plugins:grafana-irm-app:schedules-editor" {
		t.Fatalf("unexpected role entitlement: %+v", g.Entitlement.Resource.Id)
	}
	if g.Principal.Id.ResourceType != resourceTypeTeam.Id || g.Principal.Id.Resource != "7" {
		t.Fatalf("unexpected principal (team): %+v", g.Principal.Id)
	}
	annos := annotations.Annotations(g.Annotations)
	var expandable v2.GrantExpandable
	ok, err := annos.Pick(&expandable)
	if err != nil || !ok {
		t.Fatal("expected GrantExpandable annotation on team→role grant")
	}
	if !expandable.Shallow {
		t.Fatal("expected GrantExpandable.Shallow=true")
	}
	if !annos.Contains(&v2.GrantImmutable{}) {
		t.Fatal("team→role grants must be GrantImmutable (C1 cannot provision non-user principals)")
	}
	wantMemberEnt := ent.NewEntitlementID(
		&v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: "7"}},
		teamMemberEntitlement,
	)
	if len(expandable.EntitlementIds) != 1 || expandable.EntitlementIds[0] != wantMemberEnt {
		t.Fatalf("expandable entitlement ids=%v want [%s]", expandable.EntitlementIds, wantMemberEnt)
	}
}

// GrantsForResourceType must page the same way team List does: a full page of
// ResourcesPageSize yields a next token; a short page ends the loop. A silent
// NextPageToken="" after page 1 would drop every team→role grant past the first
// page of teams.
func TestRoleGrantsForResourceTypePagination(t *testing.T) {
	pageSize := int(ResourcesPageSize)
	var pages []string
	var searchCalls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/teams/search":
			page := r.URL.Query().Get("page")
			pages = append(pages, page)
			n := pageSize
			id := 1
			pageNum := 1
			if page == "2" {
				n = 1
				id = 2
				pageNum = 2
			}
			items := make([]map[string]any, 0, n)
			for i := 0; i < n; i++ {
				itemID := id
				if page != "2" && i > 0 {
					itemID = 1000 + i
				}
				items = append(items, map[string]any{
					"id": itemID, "uid": "team-" + strconv.Itoa(itemID), "orgId": 1, "name": "Ops",
				})
			}
			writeJSON(w, http.StatusOK, map[string]any{
				"page": pageNum, "perPage": pageSize, "teams": items,
			})
		case r.Method == http.MethodPost && r.URL.Path == "/api/access-control/teams/roles/search":
			searchCalls++
			var req struct {
				TeamIDs []int `json:"teamIds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode search body: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			out := map[string]any{}
			for _, id := range req.TeamIDs {
				key := strconv.Itoa(id)
				// One IRM role on team 1 (page 1) and team 2 (page 2) so both
				// pages contribute a grant we can count.
				if id == 1 || id == 2 {
					out[key] = []map[string]any{
						{"uid": "r" + key, "name": "plugins:grafana-irm-app:schedules-editor", "displayName": "Schedules Editor"},
					}
				} else {
					out[key] = []map[string]any{}
				}
			}
			writeJSON(w, http.StatusOK, out)
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	builder := newRoleBuilder(newCloudClientForTest(t, ts))
	opts := rs.SyncOpAttrs{}
	var principals []string
	for {
		grants, res, err := builder.GrantsForResourceType(context.Background(), resourceTypeRole.Id, opts)
		if err != nil {
			t.Fatalf("GrantsForResourceType: %v", err)
		}
		for _, g := range grants {
			principals = append(principals, g.Principal.Id.Resource)
		}
		if res == nil || res.NextPageToken == "" {
			break
		}
		opts.PageToken = pagination.Token{Token: res.NextPageToken}
	}

	if strings.Join(pages, ",") != "1,2" {
		t.Fatalf("team pages=%v want 1,2", pages)
	}
	if searchCalls != 2 {
		t.Fatalf("expected one RBAC search per team page, got %d", searchCalls)
	}
	if len(principals) != 2 || principals[0] != "1" || principals[1] != "2" {
		t.Fatalf("principals=%v want [1 2]", principals)
	}
}

// The role type is OptIn-off by default, so the SDK never schedules the
// type-scoped grants op — team membership must still sync on every edition with
// no dependency on the access-control API.
func TestTeamGrantsMemberOnly(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/api/teams/7/members" {
			writeJSON(w, http.StatusOK, []map[string]any{
				{"orgId": 1, "teamId": 7, "userId": 14, "email": "a@ex.com", "login": "alice", "name": "Alice", "permission": 0},
			})
			return
		}
		http.NotFound(w, r)
	}))
	defer ts.Close()

	resource := &v2.Resource{Id: &v2.ResourceId{ResourceType: resourceTypeTeam.Id, Resource: "7"}}
	grants, _, _, err := newTeamBuilder(newCloudClientForTest(t, ts)).Grants(context.Background(), resource, &pagination.Token{})
	if err != nil {
		t.Fatalf("Grants: %v", err)
	}
	if len(grants) != 1 || grants[0].Principal.Id.Resource != "14" {
		t.Fatalf("expected only the member grant for user 14, got %+v", grants)
	}
}

// The type-scoped grants op only runs when roles are opted in, which requires a
// reachable access-control API. Any error from the batch search must fail closed
// (never emit an empty set that C1 reads as a mass revoke of team→role grants).
func TestRoleGrantsForResourceTypeFailsClosed(t *testing.T) {
	statuses := []int{http.StatusNotFound, http.StatusForbidden, http.StatusInternalServerError}
	for _, code := range statuses {
		t.Run(strconv.Itoa(code), func(t *testing.T) {
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/api/teams/search":
					writeJSON(w, http.StatusOK, map[string]any{
						"teams": []map[string]any{{"id": 7, "name": "Ops", "orgId": 1}},
					})
				case "/api/access-control/teams/roles/search":
					writeJSON(w, code, map[string]string{"message": "nope"})
				default:
					http.NotFound(w, r)
				}
			}))
			defer ts.Close()

			_, _, err := newRoleBuilder(newCloudClientForTest(t, ts)).GrantsForResourceType(context.Background(), resourceTypeRole.Id, rs.SyncOpAttrs{})
			if err == nil {
				t.Fatalf("expected team-roles search %d to fail GrantsForResourceType", code)
			}
		})
	}
}
