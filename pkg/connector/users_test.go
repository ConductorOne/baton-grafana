package connector

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
)

func TestListCloud_CreatesUserResources(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{
			{
				ID:    42,
				Login: "alice",
				Email: "alice@example.com",
				Name:  "Alice Smith",
				Role:  roleViewer,
			},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	builder := newUserBuilder(newCloudClientForTest(t, ts))
	resources, nextToken, _, err := builder.List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List returned unexpected error: %v", err)
	}
	if nextToken != "" {
		t.Errorf("expected empty next token (no pagination in Cloud mode), got %q", nextToken)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}

	r := resources[0]
	if r.Id.Resource != "42" {
		t.Errorf("expected resource ID '42', got %q", r.Id.Resource)
	}
	if r.DisplayName != "alice" {
		// userResource uses Login as the display name
		t.Errorf("expected display name 'alice' (Login), got %q", r.DisplayName)
	}
}

func TestListCloud_DisabledUserHasDisabledStatus(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{
			{ID: 99, Login: "bob", Email: "bob@example.com", Role: roleViewer, IsDisabled: true},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	builder := newUserBuilder(newCloudClientForTest(t, ts))
	resources, _, _, err := builder.List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List returned unexpected error: %v", err)
	}
	if len(resources) != 1 {
		t.Fatalf("expected 1 resource, got %d", len(resources))
	}

	userTrait, err := rs.GetUserTrait(resources[0])
	if err != nil {
		t.Fatalf("GetUserTrait: %v", err)
	}
	if userTrait.GetStatus().GetStatus() != v2.UserTrait_Status_STATUS_DISABLED {
		t.Errorf("expected STATUS_DISABLED for disabled user, got %v", userTrait.GetStatus().GetStatus())
	}
}

func TestListCloud_ReturnsEmptyForEmptyOrg(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	builder := newUserBuilder(newCloudClientForTest(t, ts))
	resources, nextToken, _, err := builder.List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List returned unexpected error: %v", err)
	}
	if len(resources) != 0 {
		t.Errorf("expected 0 resources, got %d", len(resources))
	}
	if nextToken != "" {
		t.Errorf("expected empty next token, got %q", nextToken)
	}
}
