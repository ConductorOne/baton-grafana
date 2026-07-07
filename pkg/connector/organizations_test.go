package connector

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
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

// writeJSON writes data as JSON with the given HTTP status code.
func writeJSON(w http.ResponseWriter, status int, data interface{}) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(data)
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

// is_externally_synced comes from the native flag or is derived from auth labels.
func TestUserResource_SurfacesExternalSyncOnProfile(t *testing.T) {
	cases := []struct {
		name           string
		user           grafana.User
		wantSynced     bool
		wantAuthLabels string
	}{
		{
			name:           "cloud flag set",
			user:           grafana.User{ID: 42, Login: "alice", IsExternallySynced: true, AuthLabels: []string{"grafana.com"}},
			wantSynced:     true,
			wantAuthLabels: "grafana.com",
		},
		{
			name:           "self-hosted derives from auth labels",
			user:           grafana.User{ID: 43, Login: "keycloak-user", IsExternallySynced: false, AuthLabels: []string{"Generic OAuth"}},
			wantSynced:     true,
			wantAuthLabels: "Generic OAuth",
		},
		{
			name:           "local user",
			user:           grafana.User{ID: 44, Login: "bob"},
			wantSynced:     false,
			wantAuthLabels: "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r, err := userResource(&tc.user)
			if err != nil {
				t.Fatalf("userResource returned unexpected error: %v", err)
			}

			ut := &v2.UserTrait{}
			annos := annotations.Annotations(r.Annotations)
			if ok, err := annos.Pick(ut); err != nil || !ok {
				t.Fatalf("user resource missing UserTrait (ok=%v, err=%v)", ok, err)
			}
			fields := ut.GetProfile().GetFields()
			if got := fields["is_externally_synced"].GetBoolValue(); got != tc.wantSynced {
				t.Errorf("is_externally_synced = %v, want %v", got, tc.wantSynced)
			}
			if got := fields["auth_labels"].GetStringValue(); got != tc.wantAuthLabels {
				t.Errorf("auth_labels = %q, want %q", got, tc.wantAuthLabels)
			}
		})
	}
}
