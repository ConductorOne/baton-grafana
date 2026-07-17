package connector

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"
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

// TestCreateAccountCloud_LoginDisabledReturnsActionableError reproduces CXH-2012:
// a default-configuration Grafana Cloud instance (basic login form disabled) rejects
// invites for brand-new external users with HTTP 400. The connector must surface an
// actionable error tagged with grafana.ErrExternalUserLoginDisabled, not an opaque 400.
func TestCreateAccountCloud_LoginDisabledReturnsActionableError(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/invites", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusBadRequest, grafana.GrafanaError{
			ErrorMessage: "Cannot invite external user when login is disabled.",
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	profile, err := structpb.NewStruct(map[string]any{
		profileFieldEmail:    "jane.doe@example.com",
		profileFieldFullName: "Jane Doe",
	})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}

	builder := newUserBuilder(newCloudClientForTest(t, ts))
	_, _, _, err = builder.CreateAccount(context.Background(), &v2.AccountInfo{Profile: profile}, nil)
	if err == nil {
		t.Fatal("expected CreateAccount to fail when login is disabled, got nil error")
	}
	if !errors.Is(err, grafana.ErrExternalUserLoginDisabled) {
		t.Errorf("expected error to wrap grafana.ErrExternalUserLoginDisabled, got %v", err)
	}
	if !strings.Contains(err.Error(), "SCIM") {
		t.Errorf("expected actionable error mentioning SCIM, got %v", err)
	}
	// The failure is a terminal configuration prerequisite: the platform must see
	// InvalidArgument (non-retryable), not an opaque codes.Unknown from a bare fmt.Errorf.
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("expected gRPC status codes.InvalidArgument, got %v", code)
	}
}

// TestCreateAccountCloud_Non400WithSameMessageIsNotTagged guards the 400 status
// gate: an error that carries the "login is disabled" phrase but is NOT a 400
// (e.g. a 500) must fall through to the generic path, not be mis-tagged as
// ErrExternalUserLoginDisabled.
func TestCreateAccountCloud_Non400WithSameMessageIsNotTagged(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/invites", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusInternalServerError, grafana.GrafanaError{
			ErrorMessage: "Cannot invite external user when login is disabled.",
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	profile, err := structpb.NewStruct(map[string]any{
		profileFieldEmail:    "jane.doe@example.com",
		profileFieldFullName: "Jane Doe",
	})
	if err != nil {
		t.Fatalf("structpb.NewStruct: %v", err)
	}

	builder := newUserBuilder(newCloudClientForTest(t, ts))
	_, _, _, err = builder.CreateAccount(context.Background(), &v2.AccountInfo{Profile: profile}, nil)
	if err == nil {
		t.Fatal("expected CreateAccount to fail on a 500, got nil error")
	}
	if errors.Is(err, grafana.ErrExternalUserLoginDisabled) {
		t.Errorf("a non-400 error must not be tagged ErrExternalUserLoginDisabled, got %v", err)
	}
}

// TestListCloud_ExternalSyncMirrorsNativeFlag reproduces CXH-2063 through the Cloud
// List path. The org-users endpoint returns the authoritative native IsExternallySynced
// flag, so is_externally_synced must mirror it exactly — even though every Cloud user
// carries authLabels:["grafana.com"]. Before the fix the authLabels OR'd every user to
// true, hiding instance-managed access. Mirrors the ticket's TTD-shaped mock: two
// genuinely IdP-synced identities (native true) and two instance-managed admins (native
// false), all with grafana.com auth labels.
func TestListCloud_ExternalSyncMirrorsNativeFlag(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/org/users", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, http.StatusOK, []grafana.UserByOrgResponse{
			{ID: 1, Login: "brad.thater", Email: "brad@ttd.com", Role: roleAdmin, IsExternallySynced: true, AuthLabels: []string{"grafana.com"}},
			{ID: 2, Login: "svc_scim", Email: "scim@ttd.com", Role: roleAdmin, IsExternallySynced: true, AuthLabels: []string{"grafana.com"}},
			{ID: 3, Login: "alice.instance", Email: "alice@ttd.com", Role: roleAdmin, IsExternallySynced: false, AuthLabels: []string{"grafana.com"}},
			{ID: 4, Login: "bob.instance", Email: "bob@ttd.com", Role: roleAdmin, IsExternallySynced: false, AuthLabels: []string{"grafana.com"}},
		})
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	builder := newUserBuilder(newCloudClientForTest(t, ts))
	resources, _, _, err := builder.List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List returned unexpected error: %v", err)
	}
	if len(resources) != 4 {
		t.Fatalf("expected 4 resources, got %d", len(resources))
	}

	// is_externally_synced mirrors the native flag verbatim: instance-managed admins
	// report false even though they carry grafana.com auth labels.
	want := map[string]bool{"1": true, "2": true, "3": false, "4": false}
	for _, r := range resources {
		ut, err := rs.GetUserTrait(r)
		if err != nil {
			t.Fatalf("GetUserTrait for %s: %v", r.Id.Resource, err)
		}
		fields := ut.GetProfile().GetFields()
		if _, present := fields["is_externally_synced"]; !present {
			t.Errorf("user %s (%s): is_externally_synced missing; Cloud org-users always returns the flag", r.Id.Resource, r.DisplayName)
			continue
		}
		if got := fields["is_externally_synced"].GetBoolValue(); got != want[r.Id.Resource] {
			t.Errorf("user %s (%s): is_externally_synced = %v, want %v", r.Id.Resource, r.DisplayName, got, want[r.Id.Resource])
		}
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
