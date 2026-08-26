package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
)

// On OSS every /api/access-control path answers 404, so each RBAC call maps it
// to ErrRBACUnavailable on its own instead of relying on a shared probe.
func TestRBACEndpointsMapNotFoundToUnavailable(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Not found"})
	}))
	defer ts.Close()

	client, err := NewClient(context.Background(), ts.URL, "", "", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	if _, _, err := client.ListRoles(ctx); !errors.Is(err, ErrRBACUnavailable) {
		t.Fatalf("ListRoles: %v", err)
	}
	if _, _, err := client.ListRolesForTeam(ctx, 7); !errors.Is(err, ErrRBACUnavailable) {
		t.Fatalf("ListRolesForTeam: %v", err)
	}
	if calls != 2 {
		t.Fatalf("expected one call per method, got %d", calls)
	}
}

// A credential without `roles:read` / `teams.roles:read` gets 403 on every
// RBAC path. That is a distinct condition from an absent API, so it maps to
// its own sentinel and each caller decides: the role List fails, the team's
// role path skips.
func TestRBACEndpointsMapForbiddenToForbidden(t *testing.T) {
	var calls int
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "Access denied"})
	}))
	defer ts.Close()

	client, err := NewClient(context.Background(), ts.URL, "", "", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	ctx := context.Background()

	if _, _, err := client.ListRoles(ctx); !errors.Is(err, ErrRBACForbidden) {
		t.Fatalf("ListRoles: %v", err)
	}
	_, _, teamsErr := client.ListRolesForTeam(ctx, 7)
	if !errors.Is(teamsErr, ErrRBACForbidden) {
		t.Fatalf("ListRolesForTeam: %v", teamsErr)
	}
	if errors.Is(teamsErr, ErrRBACUnavailable) {
		t.Fatal("403 must not map to ErrRBACUnavailable")
	}
	if calls != 2 {
		t.Fatalf("expected one call per method, got %d", calls)
	}
}

// A 5xx must stay a plain error so the sync fails closed rather than treating
// the RBAC API as absent.
func TestRBACEndpointsServerErrorIsNotUnavailable(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusInternalServerError)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "boom"})
	}))
	defer ts.Close()

	client, err := NewClient(context.Background(), ts.URL, "", "", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	_, _, err = client.ListRolesForTeam(context.Background(), 7)
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrRBACUnavailable) {
		t.Fatal("500 must not map to ErrRBACUnavailable")
	}
}

func TestListRolesForTeam(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/api/access-control/teams/2/roles" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
			http.NotFound(w, r)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode([]map[string]any{
			{"uid": "u1", "name": "plugins:grafana-irm-app:schedules-editor", "displayName": "Schedules Editor"},
		})
	}))
	defer ts.Close()

	client, err := NewClient(context.Background(), ts.URL, "", "", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, _, err := client.ListRolesForTeam(context.Background(), 2)
	if err != nil {
		t.Fatalf("ListRolesForTeam: %v", err)
	}
	if len(got) != 1 || got[0].Name != "plugins:grafana-irm-app:schedules-editor" {
		t.Fatalf("roles=%v", got)
	}
}

func TestListRolesForTeamNullBody(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte("null"))
	}))
	defer ts.Close()

	client, err := NewClient(context.Background(), ts.URL, "", "", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, _, err := client.ListRolesForTeam(context.Background(), 2)
	if err != nil {
		t.Fatalf("ListRolesForTeam: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("null body must decode as no roles, got %d", len(got))
	}
}

// ListTeams owns the vendor's 1-based paging: an unset page is normalized to 1
// and a full page yields the next page as a token string.
func TestListTeamsNormalizesPageAndReturnsToken(t *testing.T) {
	t.Parallel()

	var gotQuery string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/teams/search" {
			t.Errorf("path=%s", r.URL.Path)
		}
		gotQuery = r.URL.RawQuery
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"totalCount": 2,
			"teams": []map[string]any{
				{"id": 1, "uid": "t1", "name": "team-a", "orgId": 1},
				{"id": 2, "uid": "t2", "name": "team-b", "orgId": 1},
			},
		})
	}))
	t.Cleanup(ts.Close)

	client, err := NewClient(context.Background(), ts.URL, "", "", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	teams, next, annos, err := client.ListTeams(context.Background(), &PaginationVars{Size: 2})
	if err != nil {
		t.Fatalf("ListTeams: %v", err)
	}
	if len(teams) != 2 || teams[0].Name != "team-a" {
		t.Fatalf("teams=%v", teams)
	}
	if !strings.Contains(gotQuery, "page=1") {
		t.Fatalf("query=%q, want page normalized to 1", gotQuery)
	}
	if next != "2" {
		t.Fatalf("nextPage=%q, want %q", next, "2")
	}
	if !annos.Contains(&v2.RateLimitDescription{}) {
		t.Fatal("ListTeams must return rate-limit annotations")
	}
}

func TestListServiceAccountsAcceptsObjectAndArray(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body any
		want int
	}{
		{
			name: "paginated object",
			body: map[string]any{
				"totalCount": 1,
				"page":       1,
				"perPage":    100,
				"serviceAccounts": []map[string]any{
					{"id": 9, "name": "sa-ci", "login": "sa-9", "role": "Viewer"},
				},
			},
			want: 1,
		},
		{
			name: "bare array",
			body: []any{},
			want: 0,
		},
		{
			name: "bare array with items",
			body: []map[string]any{
				{"id": 3, "name": "sa-mock", "login": "sa-3", "role": "Editor"},
			},
			want: 1,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/serviceaccounts/search" {
					t.Errorf("path=%s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			t.Cleanup(ts.Close)

			client, err := NewClient(context.Background(), ts.URL, "", "", "token")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}

			accounts, next, annos, err := client.ListServiceAccounts(context.Background(), &PaginationVars{Page: 1, Size: 100})
			if err != nil {
				t.Fatalf("ListServiceAccounts: %v", err)
			}
			if len(accounts) != tc.want {
				t.Fatalf("got %d accounts, want %d", len(accounts), tc.want)
			}
			if next != "" {
				t.Fatalf("nextPage=%q, want empty", next)
			}
			if !annos.Contains(&v2.RateLimitDescription{}) {
				t.Fatal("ListServiceAccounts must return rate-limit annotations")
			}
		})
	}
}

func TestListRolesAcceptsArrayAndObjectWrapper(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		body any
		want int
	}{
		{
			name: "role catalog array",
			body: []map[string]any{
				{"uid": "r1", "name": "fixed:irm:admin", "displayName": "Admin", "group": "IRM"},
			},
			want: 1,
		},
		{
			name: "empty array",
			body: []any{},
			want: 0,
		},
		{
			name: "permissions wrapper object",
			body: map[string]any{
				"permissions": []any{},
				"total":       0,
				"startAt":     0,
				"maxResults":  0,
			},
			want: 0,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/access-control/roles" {
					t.Errorf("path=%s", r.URL.Path)
				}
				w.Header().Set("Content-Type", "application/json")
				_ = json.NewEncoder(w).Encode(tc.body)
			}))
			t.Cleanup(ts.Close)

			client, err := NewClient(context.Background(), ts.URL, "", "", "token")
			if err != nil {
				t.Fatalf("NewClient: %v", err)
			}

			roles, annos, err := client.ListRoles(context.Background())
			if err != nil {
				t.Fatalf("ListRoles: %v", err)
			}
			if len(roles) != tc.want {
				t.Fatalf("got %d roles, want %d", len(roles), tc.want)
			}
			if !annos.Contains(&v2.RateLimitDescription{}) {
				t.Fatal("ListRoles must return rate-limit annotations")
			}
		})
	}
}

func TestCreateUserAlreadyExistsMaps412(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/admin/users" || r.Method != http.MethodPost {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusPreconditionFailed)
		_ = json.NewEncoder(w).Encode(map[string]string{
			"message": "User with email 'e2euser@example.com' or username 'e2euser' already exists",
		})
	}))
	t.Cleanup(ts.Close)

	client, err := NewClient(context.Background(), ts.URL, "admin", "admin", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _, err = client.CreateUser(context.Background(), &CreateUserRequest{
		Name: "e2euser", Email: "e2euser@example.com", Login: "e2euser", Password: "secret",
	})
	if !errors.Is(err, ErrUserAlreadyExists) {
		t.Fatalf("412 must map to ErrUserAlreadyExists, got %v", err)
	}
}

func TestCreateUserNon412InvalidArgumentIsNotAlreadyExists(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_ = json.NewEncoder(w).Encode(map[string]string{"message": "invalid password"})
	}))
	t.Cleanup(ts.Close)

	client, err := NewClient(context.Background(), ts.URL, "admin", "admin", "")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	_, _, err = client.CreateUser(context.Background(), &CreateUserRequest{
		Name: "x", Email: "x@example.com", Login: "x", Password: "secret",
	})
	if err == nil {
		t.Fatal("expected error")
	}
	if errors.Is(err, ErrUserAlreadyExists) {
		t.Fatal("400 must not map to ErrUserAlreadyExists")
	}
}

func TestListRolesRejectsUnknownObject(t *testing.T) {
	t.Parallel()
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"roles": []any{}})
	}))
	t.Cleanup(ts.Close)

	client, err := NewClient(context.Background(), ts.URL, "", "", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if _, _, err := client.ListRoles(context.Background()); err == nil {
		t.Fatal("expected error for unrecognized object shape")
	}
}
