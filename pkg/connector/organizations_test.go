package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
)

// newCloudClientForTest creates a grafana.Client in Cloud mode (Bearer auth) pointed at ts.
func newCloudClientForTest(t *testing.T, ts *httptest.Server) *grafana.Client {
	t.Helper()
	client, err := grafana.NewClient(context.Background(), ts.URL, "", "", "test-api-token")
	if err != nil {
		t.Fatalf("newCloudClientForTest: %v", err)
	}
	return client
}

// testOrgEntitlement builds a minimal v2.Entitlement for the given org and role.
// The ID format "{type}:{orgID}:{role}" matches what Grant/Revoke parse.
func testOrgEntitlement(orgID, role string) *v2.Entitlement {
	return &v2.Entitlement{
		Id: fmt.Sprintf("%s:%s:%s", resourceTypeOrg.Id, orgID, role),
		Resource: &v2.Resource{
			Id: &v2.ResourceId{
				ResourceType: resourceTypeOrg.Id,
				Resource:     orgID,
			},
		},
	}
}

// testGrant builds a minimal v2.Grant for testing Revoke.
func testGrant(userID, orgID, role string) *v2.Grant {
	return &v2.Grant{
		Entitlement: testOrgEntitlement(orgID, role),
		Principal: &v2.Resource{
			Id: &v2.ResourceId{
				ResourceType: resourceTypeUser.Id,
				Resource:     userID,
			},
		},
	}
}

// newSelfHostedClientForTest creates a grafana.Client in self-hosted mode (basic auth,
// empty apiToken so IsCloud() is false) pointed at ts.
func newSelfHostedClientForTest(t *testing.T, ts *httptest.Server) *grafana.Client {
	t.Helper()
	client, err := grafana.NewClient(context.Background(), ts.URL, "admin", "admin", "")
	if err != nil {
		t.Fatalf("newSelfHostedClientForTest: %v", err)
	}
	return client
}

// writeJSON writes data as JSON with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
}

// writeRaw writes a raw JSON body verbatim, for when a test must control the exact wire
// shape — e.g. a key being absent from the object, not just present with its zero value.
func writeRaw(w http.ResponseWriter, status int, body string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write([]byte(body))
}

// ---------------------------------------------------------------------------
// isExternallySyncedRoleError
// ---------------------------------------------------------------------------

func TestIsExternallySyncedRoleError(t *testing.T) {
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{
			name: "exact Grafana message",
			err:  errors.New("cannot change role for externally synced user"),
			want: true,
		},
		{
			name: "message wrapped in connector prefix",
			err: fmt.Errorf("grafana-client: update org user role: %w",
				errors.New("cannot change role for externally synced user")),
			want: true,
		},
		{
			name: "unrelated 403",
			err:  errors.New("request failed with status 403: permission denied"),
			want: false,
		},
		{
			name: "generic internal error",
			err:  errors.New("internal server error"),
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isExternallySyncedRoleError(tc.err)
			if got != tc.want {
				t.Errorf("isExternallySyncedRoleError(%q) = %v, want %v", tc.err, got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// grantCloud
// ---------------------------------------------------------------------------

func TestGrantCloud_IdempotentWhenSameRole(t *testing.T) {
	// Arrange: user 42 already has Viewer role — PATCH must not be called.
	patchCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{
			{ID: 42, Login: "alice", Email: "alice@example.com", Role: roleViewer},
		})
	})
	mux.HandleFunc("/api/org/users/42", func(w http.ResponseWriter, r *http.Request) {
		patchCalled = true
		http.Error(w, "should not be called", http.StatusInternalServerError)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	principal, err := userResource(&grafana.User{ID: 42, Login: "alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("userResource: %v", err)
	}

	annos, err := newOrgBuilder(newCloudClientForTest(t, ts)).Grant(
		context.Background(), principal, testOrgEntitlement("1", roleViewer),
	)
	if err != nil {
		t.Fatalf("Grant returned unexpected error: %v", err)
	}
	if len(annos) == 0 {
		t.Error("expected GrantAlreadyExists annotation, got none")
	}
	if patchCalled {
		t.Error("PATCH should not be called when user already has the requested role")
	}
}

func TestGrantCloud_RoleChangeSuccess(t *testing.T) {
	// Arrange: user 42 has Viewer role; grant Editor — PATCH succeeds.
	patchCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{
			{ID: 42, Login: "alice", Email: "alice@example.com", Role: roleViewer, IsExternallySynced: false},
		})
	})
	mux.HandleFunc("/api/org/users/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodPatch {
			patchCalled = true
			writeJSON(w, http.StatusOK, map[string]string{"message": "Organization user updated"})
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	principal, err := userResource(&grafana.User{ID: 42, Login: "alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("userResource: %v", err)
	}

	annos, err := newOrgBuilder(newCloudClientForTest(t, ts)).Grant(
		context.Background(), principal, testOrgEntitlement("1", roleEditor),
	)
	if err != nil {
		t.Fatalf("Grant returned unexpected error: %v", err)
	}
	if len(annos) != 0 {
		t.Error("expected no annotations for successful role change")
	}
	if !patchCalled {
		t.Error("expected PATCH /api/org/users/42 to be called")
	}
}

func TestGrantCloud_ExternallySyncedRoleChange(t *testing.T) {
	// Arrange: user 42 is externally synced; PATCH returns 403 with Grafana's error.
	patchCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{
			{
				ID: 42, Login: "alice", Email: "alice@example.com", Role: roleViewer,
				IsExternallySynced: true, AuthLabels: []string{"grafana_com"},
			},
		})
	})
	mux.HandleFunc("/api/org/users/42", func(w http.ResponseWriter, r *http.Request) {
		patchCalled = true
		writeJSON(w, http.StatusForbidden, map[string]string{
			"message": "cannot change role for externally synced user",
			"status":  "error",
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	principal, err := userResource(&grafana.User{ID: 42, Login: "alice", Email: "alice@example.com"})
	if err != nil {
		t.Fatalf("userResource: %v", err)
	}

	_, gotErr := newOrgBuilder(newCloudClientForTest(t, ts)).Grant(
		context.Background(), principal, testOrgEntitlement("1", roleEditor),
	)
	if gotErr == nil {
		t.Fatal("expected error for externally synced user role change, got nil")
	}
	if !patchCalled {
		t.Error("expected PATCH /api/org/users/42 to be called")
	}
	if !strings.Contains(gotErr.Error(), "external identity provider") {
		t.Errorf("error message should mention 'external identity provider', got: %s", gotErr)
	}
}

// ---------------------------------------------------------------------------
// revokeCloud
// ---------------------------------------------------------------------------

func TestRevokeCloud_IdempotentWhenUserNotInOrg(t *testing.T) {
	// Arrange: empty org — user is not a member.
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	annos, err := newOrgBuilder(newCloudClientForTest(t, ts)).Revoke(
		context.Background(), testGrant("42", "1", roleViewer),
	)
	if err != nil {
		t.Fatalf("Revoke returned unexpected error: %v", err)
	}
	if len(annos) == 0 {
		t.Error("expected GrantAlreadyRevoked annotation, got none")
	}
}

func TestRevokeCloud_RemovesUser(t *testing.T) {
	// Arrange: user 42 is in the org; DELETE should be called.
	deleteCalled := false
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{
			{ID: 42, Login: "alice", Email: "alice@example.com", Role: roleViewer},
		})
	})
	mux.HandleFunc("/api/org/users/42", func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodDelete {
			deleteCalled = true
			writeJSON(w, http.StatusOK, map[string]string{"message": "User removed from organization"})
		}
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	annos, err := newOrgBuilder(newCloudClientForTest(t, ts)).Revoke(
		context.Background(), testGrant("42", "1", roleViewer),
	)
	if err != nil {
		t.Fatalf("Revoke returned unexpected error: %v", err)
	}
	if len(annos) != 0 {
		t.Error("expected no annotations for successful revoke")
	}
	if !deleteCalled {
		t.Error("expected DELETE /api/org/users/42 to be called")
	}
}

// is_externally_synced surfaces Grafana's native isExternallySynced flag verbatim and
// ONLY when Grafana returns it (User.IsExternallySynced != nil). It is never derived
// from auth labels: an instance-managed Cloud admin with native false and
// authLabels:["grafana.com"] reports false, and a self-hosted user
// whose /api/users response omits the flag has the field left off the profile entirely.
// See userResource for the full rationale.
func TestUserResource_SurfacesExternalSyncOnProfile(t *testing.T) {
	bp := func(b bool) *bool { return &b }
	cases := []struct {
		name              string
		user              grafana.User
		wantSyncedPresent bool
		wantSynced        bool
		wantAuthLabels    string
	}{
		{
			// Cloud, org role managed by an external IdP: native flag true → surfaced true.
			name:              "cloud native true",
			user:              grafana.User{ID: 42, Login: "alice", IsExternallySynced: bp(true), AuthLabels: []string{"grafana.com"}},
			wantSyncedPresent: true,
			wantSynced:        true,
			wantAuthLabels:    "grafana.com",
		},
		{
			// Core case: in Cloud every user authenticates through grafana.com SSO and
			// carries authLabels:["grafana.com"], but an instance-managed admin's native flag
			// is false. The field must mirror the native flag (false), NOT be OR'd to true by
			// the auth labels (the pre-fix bug).
			name:              "cloud native false with grafana.com auth labels reports false",
			user:              grafana.User{ID: 45, Login: "alice.instance", IsExternallySynced: bp(false), AuthLabels: []string{"grafana.com"}},
			wantSyncedPresent: true,
			wantSynced:        false,
			wantAuthLabels:    "grafana.com",
		},
		{
			// Self-hosted: /api/users omits the native flag (nil pointer) → field absent.
			// auth_labels is still surfaced separately (it is a different signal).
			name:              "self-hosted flag absent, field omitted",
			user:              grafana.User{ID: 43, Login: "keycloak-user", IsExternallySynced: nil, AuthLabels: []string{"Generic OAuth"}},
			wantSyncedPresent: false,
			wantAuthLabels:    "Generic OAuth",
		},
		{
			// Self-hosted local user: no flag, no auth labels → neither field present.
			name:              "self-hosted local user, both absent",
			user:              grafana.User{ID: 44, Login: "bob", IsExternallySynced: nil},
			wantSyncedPresent: false,
			wantAuthLabels:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := userResource(&tc.user)
			if err != nil {
				t.Fatalf("userResource returned unexpected error: %v", err)
			}

			fields := rs.GetProfile(r).GetFields()
			if _, present := fields["is_externally_synced"]; present != tc.wantSyncedPresent {
				t.Errorf("is_externally_synced present = %v, want %v", present, tc.wantSyncedPresent)
			}
			if tc.wantSyncedPresent {
				if got := fields["is_externally_synced"].GetBoolValue(); got != tc.wantSynced {
					t.Errorf("is_externally_synced = %v, want %v", got, tc.wantSynced)
				}
			}
			if got := fields["auth_labels"].GetStringValue(); got != tc.wantAuthLabels {
				t.Errorf("auth_labels = %q, want %q", got, tc.wantAuthLabels)
			}
		})
	}
}

// TestListSelfHosted_Orgs_SingleOrgSyncs verifies the single-organization path.
// /api/orgs is 0-based, so the first page must be requested with no explicit page
// param (page 0). A fresh Grafana has exactly one org (id 1); requesting page=1
// (as a 1-based scheme would) returns an empty second page, so the org — and its
// org:1:Admin entitlement — never syncs, and the grant e2e fails with
// "error fetching entitlement 'org:1:Admin': sql: no rows in result set".
func TestListSelfHosted_Orgs_SingleOrgSyncs(t *testing.T) {
	var requestedPages []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/orgs", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		requestedPages = append(requestedPages, page)
		// 0-based: page 0 (absent) → the single org; any later page → empty.
		if page == "" || page == "0" {
			writeJSON(w, http.StatusOK, []grafana.Organization{{ID: 1, Name: "Main Org."}})
			return
		}
		writeJSON(w, http.StatusOK, []grafana.Organization{})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	builder := newOrgBuilder(newSelfHostedClientForTest(t, ts))
	resources, next, _, err := builder.List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List returned unexpected error: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 org, got %d (regression: org sync returned nothing)", len(resources))
	}
	if resources[0].Id.Resource != "1" {
		t.Errorf("expected org ID '1', got %q", resources[0].Id.Resource)
	}
	if next != "" {
		t.Errorf("expected no next page for a single org, got %q", next)
	}
	if len(requestedPages) != 1 || (requestedPages[0] != "" && requestedPages[0] != "0") {
		t.Errorf("first /api/orgs request must be page 0 (absent), got %v", requestedPages)
	}
}

// TestListSelfHosted_Orgs_PaginationZeroBased pins the 0-based paging contract for
// /api/orgs: the first page is requested with no page param (page 0), then 1, 2, …,
// each exactly once, with no duplicate resources.
func TestListSelfHosted_Orgs_PaginationZeroBased(t *testing.T) {
	const (
		perPage  = int(ResourcesPageSize) // 50
		totalOrg = 109                    // pages 0, 1, 2 → 50 + 50 + 9
	)

	var requestedPages []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/orgs", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		requestedPages = append(requestedPages, page)

		// 0-based; an absent page param is page 0.
		pnum := 0
		if page != "" {
			p, err := strconv.Atoi(page)
			if err != nil {
				t.Errorf("server received non-numeric page %q", page)
			}
			pnum = p
		}

		start := pnum * perPage
		orgs := make([]grafana.Organization, 0, perPage)
		for id := start + 1; id <= start+perPage && id <= totalOrg; id++ {
			orgs = append(orgs, grafana.Organization{ID: id, Name: fmt.Sprintf("org%d", id)})
		}
		writeJSON(w, http.StatusOK, orgs)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	builder := newOrgBuilder(newSelfHostedClientForTest(t, ts))
	seen := map[string]bool{}
	token := &pagination.Token{}
	for i := 0; ; i++ {
		if i > 10 {
			t.Fatal("pagination did not terminate within 10 pages")
		}
		resources, next, _, err := builder.List(context.Background(), nil, token)
		if err != nil {
			t.Fatalf("List returned unexpected error: %v", err)
		}
		for _, r := range resources {
			if seen[r.Id.Resource] {
				t.Errorf("org %s returned more than once (duplicate fetch)", r.Id.Resource)
			}
			seen[r.Id.Resource] = true
		}
		if next == "" {
			break
		}
		token = &pagination.Token{Token: next}
	}

	if len(seen) != totalOrg {
		t.Errorf("expected %d unique orgs, got %d", totalOrg, len(seen))
	}
	// 0-based: first request omits the page param (page 0), then page=1, page=2.
	wantPages := []string{"", "1", "2"}
	if len(requestedPages) != len(wantPages) {
		t.Fatalf("expected pages %v to each be requested once, got %v", wantPages, requestedPages)
	}
	for i, want := range wantPages {
		if requestedPages[i] != want {
			t.Errorf("request %d: expected page=%q, got page=%q (full sequence %v)", i, want, requestedPages[i], requestedPages)
		}
	}
}
