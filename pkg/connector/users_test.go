package connector

import (
	"context"
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

// TestListCloud_ExternalSyncMirrorsNativeFlag exercises the Cloud List path.
// The org-users endpoint returns the authoritative native IsExternallySynced
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

// TestListSelfHosted_OmitsExternalSyncFromRawJSON pins the JSON-to-*bool half of the
// contract that the *bool exists for: it drives the self-hosted List path
// against a raw /api/users body that OMITS the isExternallySynced key (the real
// shape — verified against Grafana OSS). The key's absence must decode to a nil
// pointer, so userResource leaves is_externally_synced off the profile. auth_labels
// is unaffected — surfaced when present. (QE note on PR #76.)
func TestListSelfHosted_OmitsExternalSyncFromRawJSON(t *testing.T) {
	mux := http.NewServeMux()
	// Bare /api/users array with NO isExternallySynced key on any object.
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		writeRaw(w, http.StatusOK, `[
			{"id":1,"login":"admin","email":"admin@localhost","name":"","isAdmin":true,"isDisabled":false,"authLabels":[]},
			{"id":2,"login":"sso.user","email":"sso@localhost","name":"SSO User","isDisabled":false,"authLabels":["Generic OAuth"]}
		]`)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	builder := newUserBuilder(newSelfHostedClientForTest(t, ts))
	resources, _, _, err := builder.List(context.Background(), nil, &pagination.Token{})
	if err != nil {
		t.Fatalf("List returned unexpected error: %v", err)
	}
	if len(resources) != 2 {
		t.Fatalf("expected 2 resources, got %d", len(resources))
	}

	wantAuthLabels := map[string]string{"1": "", "2": "Generic OAuth"}
	for _, r := range resources {
		ut, err := rs.GetUserTrait(r)
		if err != nil {
			t.Fatalf("GetUserTrait for %s: %v", r.Id.Resource, err)
		}
		fields := ut.GetProfile().GetFields()
		if _, present := fields["is_externally_synced"]; present {
			t.Errorf("user %s: is_externally_synced must be absent when /api/users omits the key (nil *bool), got present", r.Id.Resource)
		}
		if got := fields["auth_labels"].GetStringValue(); got != wantAuthLabels[r.Id.Resource] {
			t.Errorf("user %s: auth_labels = %q, want %q", r.Id.Resource, got, wantAuthLabels[r.Id.Resource])
		}
	}
}

// TestListSelfHosted_PaginationNoDoubleFetch reproduces CXH-2013: in self-hosted mode
// the connector paginated /api/users starting at page=0 and incrementing (nextPage =
// page+1). Grafana's list endpoints are 1-based and treat page=0 as page one, so page=0
// and page=1 both returned the first page — the first page was fetched twice, inflating
// the reported sync count (109 on a 59-user tenant in the ticket) even though the bundle
// deduped by ID. The fix makes pagination 1-based: page must be requested as 1, 2, 3, …
// each exactly once.
func TestListSelfHosted_PaginationNoDoubleFetch(t *testing.T) {
	const (
		perPage    = int(ResourcesPageSize) // 50
		totalUsers = 109                    // 50 + 50 + 9 → three pages
	)

	var requestedPages []string
	mux := http.NewServeMux()
	mux.HandleFunc("/api/users", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		requestedPages = append(requestedPages, page)

		// Grafana is 1-based; an absent page param is treated as page one.
		pnum := 1
		if page != "" {
			p, err := strconv.Atoi(page)
			if err != nil {
				t.Errorf("server received non-numeric page %q", page)
			}
			pnum = p
		}

		start := (pnum - 1) * perPage
		users := make([]grafana.User, 0, perPage)
		for id := start + 1; id <= start+perPage && id <= totalUsers; id++ {
			users = append(users, grafana.User{
				ID:    id,
				Login: fmt.Sprintf("user%d", id),
				Email: fmt.Sprintf("user%d@localhost", id),
			})
		}
		writeJSON(w, http.StatusOK, users)
	})
	ts := httptest.NewServer(mux)
	defer ts.Close()

	builder := newUserBuilder(newSelfHostedClientForTest(t, ts))

	// Drive the pagination loop the way the SDK does: feed each returned next token back
	// in until it is empty.
	seen := map[string]bool{}
	token := &pagination.Token{}
	pages := 0
	for {
		pages++
		if pages > 10 {
			t.Fatal("pagination did not terminate within 10 pages")
		}
		resources, next, _, err := builder.List(context.Background(), nil, token)
		if err != nil {
			t.Fatalf("List returned unexpected error: %v", err)
		}
		for _, r := range resources {
			if seen[r.Id.Resource] {
				t.Errorf("user %s returned more than once (duplicate fetch)", r.Id.Resource)
			}
			seen[r.Id.Resource] = true
		}
		if next == "" {
			break
		}
		token = &pagination.Token{Token: next}
	}

	if len(seen) != totalUsers {
		t.Errorf("expected %d unique users, got %d", totalUsers, len(seen))
	}

	// The core assertion: every page requested exactly once, starting at 1. Before the fix
	// this was ["", "1", "2", "3"] — page one fetched twice (absent + explicit page=1).
	wantPages := []string{"1", "2", "3"}
	if len(requestedPages) != len(wantPages) {
		t.Fatalf("expected pages %v to each be requested exactly once, got %v", wantPages, requestedPages)
	}
	for i, want := range wantPages {
		if requestedPages[i] != want {
			t.Errorf("request %d: expected page=%q, got page=%q (full sequence %v)", i, want, requestedPages[i], requestedPages)
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
