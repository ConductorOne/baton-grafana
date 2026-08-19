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
	if _, _, err := client.ListRolesForTeams(ctx, []int{7}); !errors.Is(err, ErrRBACUnavailable) {
		t.Fatalf("ListRolesForTeams: %v", err)
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

	_, _, err = client.ListRolesForTeams(context.Background(), []int{7})
	if err == nil {
		t.Fatal("expected error on 500")
	}
	if errors.Is(err, ErrRBACUnavailable) {
		t.Fatal("500 must not map to ErrRBACUnavailable")
	}
}

func TestListRolesForTeams(t *testing.T) {
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/access-control/teams/roles/search":
			if r.Method != http.MethodPost {
				t.Errorf("method=%s", r.Method)
			}
			var req struct {
				TeamIDs []int `json:"teamIds"`
			}
			if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
				t.Errorf("decode: %v", err)
				http.Error(w, "bad request", http.StatusBadRequest)
				return
			}
			if len(req.TeamIDs) != 2 || req.TeamIDs[0] != 2 || req.TeamIDs[1] != 9 {
				t.Errorf("teamIds=%v", req.TeamIDs)
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{
				"2": []map[string]any{
					{"uid": "u1", "name": "plugins:grafana-irm-app:schedules-editor", "displayName": "Schedules Editor"},
				},
				"9": []map[string]any{},
			})
		default:
			http.NotFound(w, r)
		}
	}))
	defer ts.Close()

	client, err := NewClient(context.Background(), ts.URL, "", "", "token")
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}

	got, _, err := client.ListRolesForTeams(context.Background(), []int{2, 9})
	if err != nil {
		t.Fatalf("ListRolesForTeams: %v", err)
	}
	if len(got["2"]) != 1 || got["2"][0].Name != "plugins:grafana-irm-app:schedules-editor" {
		t.Fatalf("team 2 roles=%v", got["2"])
	}
	if roles, ok := got["9"]; !ok || len(roles) != 0 {
		t.Fatalf("team 9 roles=%v ok=%v", roles, ok)
	}

	empty, _, err := client.ListRolesForTeams(context.Background(), nil)
	if err != nil {
		t.Fatalf("empty ListRolesForTeams: %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("empty map len=%d", len(empty))
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
