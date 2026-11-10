package connector

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/cli"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// --- fixture ---

// tokenFixture is a fake Grafana service-account API. It records every request
// it sees so a test can assert the exact route, body and call count.
type tokenFixture struct {
	mu sync.Mutex
	// serviceAccounts is returned by GET /api/serviceaccounts/search, one page
	// at a time, honoring the request's perpage.
	serviceAccounts []*grafana.ServiceAccount
	// tokens maps a service account id to the tokens GET
	// /api/serviceaccounts/{id}/tokens returns before any create.
	tokens map[int][]*grafana.ServiceAccountToken
	// tokensAfterCreate, when set, replaces tokens once a create has happened.
	// When nil, the fixture synthesizes the token the create response describes,
	// expiring at createdExpiration.
	tokensAfterCreate map[int][]*grafana.ServiceAccountToken
	// createdExpiration is the expiry the provider reports for the token it just
	// minted. nil models a provider that reports the token as non-expiring.
	createdExpiration *time.Time
	// tokenListStatusAfterCreate, when non-zero, is the status the token list
	// answers with once a create has happened.
	tokenListStatusAfterCreate int
	// tokenListBodyAfterCreate, when set, is written verbatim once a create has
	// happened, modeling a provider whose expiry cannot be decoded.
	tokenListBodyAfterCreate string
	created                  bool
	// createdName is the name the last create carried.
	createdName string
	// createResponse, when set, is the body POST returns.
	createResponse *grafana.CreatedServiceAccountToken
	// createStatus and createBody override the create response for failure cases.
	createStatus int
	createBody   map[string]any
	// deleteStatus and deleteBody override the delete response (0 means 200).
	deleteStatus int
	deleteBody   map[string]any
	// tokenListStatus overrides the token list response status (0 means 200).
	tokenListStatus int
	// mintedNames, when non-nil, models the api_key table's real
	// UNIQUE (org_id, name) index: a create whose name is already present fails
	// the way the store's INSERT would.
	mintedNames map[string]bool
	// arrived and release form a barrier so a concurrency test's creates both
	// reach the provider before either is answered.
	arrived chan struct{}
	release chan struct{}

	requests []recordedRequest
}

type recordedRequest struct {
	method string
	path   string
	body   map[string]any
}

func (f *tokenFixture) record(r *http.Request) map[string]any {
	rec := recordedRequest{method: r.Method, path: r.URL.Path}
	var body map[string]any
	if r.Body != nil {
		if err := json.NewDecoder(r.Body).Decode(&body); err == nil {
			rec.body = body
		}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.requests = append(f.requests, rec)
	return body
}

func (f *tokenFixture) recorded() []recordedRequest {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]recordedRequest(nil), f.requests...)
}

func (f *tokenFixture) countMethod(method string) int {
	n := 0
	for _, req := range f.recorded() {
		if req.method == method {
			n++
		}
	}
	return n
}

func (f *tokenFixture) countMethodPath(method, path string) int {
	n := 0
	for _, req := range f.recorded() {
		if req.method == method && req.path == path {
			n++
		}
	}
	return n
}

func (f *tokenFixture) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body := f.record(r)

		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/api/serviceaccounts/search":
			f.writeServiceAccountsPage(w, r)
		case r.Method == http.MethodGet && strings.HasSuffix(r.URL.Path, "/tokens"):
			f.writeTokenList(w, r)
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/tokens"):
			name, _ := body["name"].(string)
			f.writeCreatedToken(w, name)
		case r.Method == http.MethodDelete && strings.Contains(r.URL.Path, "/tokens/"):
			f.writeDeleteResult(w)
		default:
			writeJSON(w, http.StatusNotFound, map[string]string{"message": "unexpected route " + r.URL.Path})
		}
	}
}

func (f *tokenFixture) writeServiceAccountsPage(w http.ResponseWriter, r *http.Request) {
	page, _ := strconv.Atoi(r.URL.Query().Get("page"))
	if page < 1 {
		page = 1
	}
	perPage, _ := strconv.Atoi(r.URL.Query().Get("perpage"))
	if perPage <= 0 {
		perPage = int(ResourcesPageSize)
	}
	start := (page - 1) * perPage
	if start > len(f.serviceAccounts) {
		start = len(f.serviceAccounts)
	}
	end := start + perPage
	if end > len(f.serviceAccounts) {
		end = len(f.serviceAccounts)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"totalCount":      len(f.serviceAccounts),
		"serviceAccounts": f.serviceAccounts[start:end],
		"page":            page,
		"perPage":         perPage,
	})
}

func (f *tokenFixture) writeTokenList(w http.ResponseWriter, r *http.Request) {
	id, err := serviceAccountIDFromTokenPath(r.URL.Path)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"message": err.Error()})
		return
	}

	f.mu.Lock()
	created := f.created
	status := f.tokenListStatus
	statusAfterCreate := f.tokenListStatusAfterCreate
	bodyAfterCreate := f.tokenListBodyAfterCreate
	afterCreate := f.tokensAfterCreate
	createdExpiration := f.createdExpiration
	createResponse := f.createResponse
	createdName := f.createdName
	f.mu.Unlock()

	if !created {
		if status != 0 {
			writeJSON(w, status, map[string]string{"message": "token list refused"})
			return
		}
		writeTokens(w, f.tokens[id])
		return
	}

	if statusAfterCreate != 0 {
		writeJSON(w, statusAfterCreate, map[string]string{"message": "token list refused after create"})
		return
	}
	if bodyAfterCreate != "" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(bodyAfterCreate))
		return
	}
	if afterCreate != nil {
		writeTokens(w, afterCreate[id])
		return
	}

	// The provider now reports the token it just minted, with the expiry it
	// derived from its own clock.
	created2 := createResponse
	if created2 == nil {
		created2 = &grafana.CreatedServiceAccountToken{ID: 41, Name: createdName, Key: "glsa_one_time_value"}
	}
	if created2.ID > 0 {
		writeTokens(w, []*grafana.ServiceAccountToken{{ID: created2.ID, Name: created2.Name, Expiration: createdExpiration}})
		return
	}
	writeTokens(w, nil)
}

func writeTokens(w http.ResponseWriter, tokens []*grafana.ServiceAccountToken) {
	if tokens == nil {
		tokens = []*grafana.ServiceAccountToken{}
	}
	writeJSON(w, http.StatusOK, tokens)
}

func (f *tokenFixture) writeCreatedToken(w http.ResponseWriter, name string) {
	if f.arrived != nil {
		f.arrived <- struct{}{}
		// Bounded: a test that never closes the gate must not wedge the
		// provider, and one issuance may legitimately never reach the create.
		select {
		case <-f.release:
		case <-time.After(2 * time.Second):
		}
	}

	f.mu.Lock()
	f.created = true
	f.createdName = name
	duplicate := false
	if f.mintedNames != nil && name != "" {
		duplicate = f.mintedNames[name]
		f.mintedNames[name] = true
	}
	f.mu.Unlock()

	if duplicate {
		// The real store's INSERT violates UNIQUE (org_id, name) and the API
		// renders the raw database error as HTTP 500 -- not the friendly 400 a
		// sequential duplicate gets.
		writeJSON(w, http.StatusInternalServerError, map[string]any{"message": "failed to add service account token"})
		return
	}

	if f.createStatus != 0 {
		body := f.createBody
		if body == nil {
			body = map[string]any{"message": "create refused"}
		}
		writeJSON(w, f.createStatus, body)
		return
	}
	created := f.createResponse
	if created == nil {
		created = &grafana.CreatedServiceAccountToken{ID: 41, Name: name, Key: "glsa_one_time_value"}
	}
	writeJSON(w, http.StatusOK, created)
}

func (f *tokenFixture) writeDeleteResult(w http.ResponseWriter) {
	if f.deleteStatus != 0 {
		body := f.deleteBody
		if body == nil {
			body = map[string]any{"message": "Failed to delete service account token"}
		}
		writeJSON(w, f.deleteStatus, body)
		return
	}
	writeJSON(w, http.StatusOK, map[string]string{"message": "Service account token deleted"})
}

func serviceAccountIDFromTokenPath(path string) (int, error) {
	trimmed := strings.TrimPrefix(path, "/api/serviceaccounts/")
	trimmed = strings.TrimSuffix(trimmed, "/tokens")
	return strconv.Atoi(trimmed)
}

func newTokenFixtureServer(t *testing.T, fixture *tokenFixture) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(fixture.handler())
	t.Cleanup(ts.Close)
	return ts
}

// newUncachedCloudClientForTest builds the same Cloud-mode client as
// newCloudClientForTest with the SDK's HTTP response cache disabled. Tests that
// assert on request counts need this: a cached GET is served without reaching
// the fixture, which would make a call-count assertion vacuous.
func newUncachedCloudClientForTest(t *testing.T, ts *httptest.Server) *grafana.Client {
	t.Helper()
	t.Setenv("BATON_HTTP_CACHE_TTL", "0")
	return newCloudClientForTest(t, ts)
}

// connectorOptsWithSyncSelection builds the connector options C1 passes when it
// has an explicit resource-type sync selection.
func connectorOptsWithSyncSelection(resourceTypeIDs []string) *cli.ConnectorOpts {
	return &cli.ConnectorOpts{SyncResourceTypeIDs: resourceTypeIDs}
}

func secretTraitOf(t *testing.T, resource *v2.Resource) *v2.SecretTrait {
	t.Helper()
	if resource == nil {
		t.Fatal("expected a secret resource")
	}
	trait := &v2.SecretTrait{}
	secretAnnotations := annotations.Annotations(resource.GetAnnotations())
	found, err := secretAnnotations.Pick(trait)
	if err != nil {
		t.Fatalf("pick secret trait: %v", err)
	}
	if !found {
		t.Fatalf("resource %q carries no SecretTrait", resource.GetId().GetResource())
	}
	return trait
}

// --- handle ---

func TestServiceAccountTokenHandleRoundTrip(t *testing.T) {
	handle := serviceAccountTokenHandle("7", 41)
	if handle != "7.41" {
		t.Fatalf("unexpected handle %q", handle)
	}
	serviceAccountID, tokenID, err := parseServiceAccountTokenHandle(handle)
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if serviceAccountID != 7 || tokenID != 41 {
		t.Fatalf("expected (7, 41), got (%d, %d)", serviceAccountID, tokenID)
	}
}

func TestParseServiceAccountTokenHandleRejectsMalformed(t *testing.T) {
	for _, handle := range []string{"", "41", "7.", ".41", "0.41", "7.0", "seven.41", "7.forty", "7.41.9", "7.41/extra"} {
		if _, _, err := parseServiceAccountTokenHandle(handle); err == nil {
			t.Fatalf("handle %q must be rejected", handle)
		}
	}
}

// --- Delete ---

func TestDeleteTreatsAnAlreadyGoneTokenAsSuccess(t *testing.T) {
	fixture := &tokenFixture{deleteStatus: http.StatusNotFound}
	builder := newServiceAccountTokenBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)))

	_, err := builder.Delete(context.Background(), &v2.ResourceId{
		ResourceType: resourceTypeServiceAccountToken.Id,
		Resource:     "7.41",
	}, nil)
	if err != nil {
		t.Fatalf("a provider that already deleted the token is the outcome the caller asked for: %v", err)
	}

	if len(fixture.recorded()) != 1 {
		t.Fatalf("expected exactly one provider call, got %d", len(fixture.recorded()))
	}
	req := fixture.recorded()[0]
	if req.method != http.MethodDelete || req.path != "/api/serviceaccounts/7/tokens/41" {
		t.Fatalf("unexpected delete request %s %s", req.method, req.path)
	}
}

func TestDeletePropagatesRealProviderFailures(t *testing.T) {
	fixture := &tokenFixture{deleteStatus: http.StatusInternalServerError}
	builder := newServiceAccountTokenBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)))

	_, err := builder.Delete(context.Background(), &v2.ResourceId{
		ResourceType: resourceTypeServiceAccountToken.Id,
		Resource:     "7.41",
	}, nil)
	if err == nil {
		t.Fatal("a provider failure that is not 'already gone' must not be reported as success")
	}
}

func TestDeleteRejectsMalformedHandles(t *testing.T) {
	fixture := &tokenFixture{}
	builder := newServiceAccountTokenBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)))

	for _, id := range []*v2.ResourceId{
		nil,
		{ResourceType: resourceTypeServiceAccountToken.Id, Resource: ""},
		{ResourceType: resourceTypeServiceAccountToken.Id, Resource: "41"},
		{ResourceType: resourceTypeServiceAccount.Id, Resource: "7.41"},
	} {
		if _, err := builder.Delete(context.Background(), id, nil); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument for %v, got %v", id, err)
		}
	}
	if n := fixture.countMethod(http.MethodDelete); n != 0 {
		t.Fatalf("a malformed handle must not reach the provider, got %d deletes", n)
	}
}

// --- List ---

func TestListWalksServiceAccountsThenTheirTokens(t *testing.T) {
	expiry := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	created := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)
	fixture := &tokenFixture{
		serviceAccounts: []*grafana.ServiceAccount{
			{ID: 7, Name: "sa-seven"},
			{ID: 8, Name: "sa-eight"},
		},
		tokens: map[int][]*grafana.ServiceAccountToken{
			7: {
				{ID: 41, Name: "c1-ticket-1", Created: &created, Expiration: &expiry},
				{ID: 42, Name: "never-expires", Created: &created},
			},
			8: {{ID: 43, Name: "c1-ticket-2", Created: &created}},
		},
	}
	builder := newServiceAccountTokenBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)))

	byHandle := map[string]*v2.Resource{}
	pageToken := ""
	for range 10 {
		resources, results, err := builder.List(context.Background(), nil, syncAttrs(pageToken))
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, resource := range resources {
			byHandle[resource.GetId().GetResource()] = resource
		}
		pageToken = nextPageToken(results)
		if pageToken == "" {
			break
		}
	}
	if pageToken != "" {
		t.Fatal("the walk did not terminate")
	}

	if len(byHandle) != 3 {
		t.Fatalf("expected 3 tokens, got %d (%v)", len(byHandle), byHandle)
	}

	expiring, ok := byHandle["7.41"]
	if !ok {
		t.Fatal("expected the token 7.41 to be synced")
	}
	if expiring.GetParentResourceId().GetResource() != "7" {
		t.Fatalf("expected parent service account 7, got %v", expiring.GetParentResourceId())
	}
	trait := secretTraitOf(t, expiring)
	if trait.GetIdentityId().GetResource() != "7" {
		t.Fatalf("the token's identity must be its owning service account, got %v", trait.GetIdentityId())
	}
	if trait.GetCredentialDetail() != serviceAccountTokenDetail {
		t.Fatalf("unexpected credential detail %q", trait.GetCredentialDetail())
	}
	if trait.GetExpiresAt() == nil || !trait.GetExpiresAt().AsTime().Equal(expiry) {
		t.Fatalf("expected the provider expiry %s, got %v", expiry, trait.GetExpiresAt())
	}

	// A token Grafana reports as non-expiring must not be given an expiry the
	// provider never asserted.
	if nonExpiring, ok := byHandle["7.42"]; ok {
		if got := secretTraitOf(t, nonExpiring).GetExpiresAt(); got != nil {
			t.Fatalf("a non-expiring token must carry no expiry, got %s", got)
		}
	} else {
		t.Fatal("expected the non-expiring token 7.42 to be synced")
	}

	if _, ok := byHandle["8.43"]; !ok {
		t.Fatal("expected the second service account's token 8.43 to be synced")
	}
}

// TestListPaginatesTheServiceAccountLevel proves the service-account level
// advances: a full page hands back a next token, and the walk only reaches the
// last account's tokens after fetching the second page.
func TestListPaginatesTheServiceAccountLevel(t *testing.T) {
	const accounts = int(ResourcesPageSize) + 1
	serviceAccounts := make([]*grafana.ServiceAccount, 0, accounts)
	for id := 1; id <= accounts; id++ {
		serviceAccounts = append(serviceAccounts, &grafana.ServiceAccount{ID: id, Name: fmt.Sprintf("sa-%d", id)})
	}
	fixture := &tokenFixture{
		serviceAccounts: serviceAccounts,
		tokens: map[int][]*grafana.ServiceAccountToken{
			accounts: {{ID: 900, Name: "c1-last"}},
		},
	}
	builder := newServiceAccountTokenBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)))

	found := map[string]bool{}
	pageToken := ""
	for range accounts + 10 {
		resources, results, err := builder.List(context.Background(), nil, syncAttrs(pageToken))
		if err != nil {
			t.Fatalf("List: %v", err)
		}
		for _, resource := range resources {
			found[resource.GetId().GetResource()] = true
		}
		pageToken = nextPageToken(results)
		if pageToken == "" {
			break
		}
	}
	if pageToken != "" {
		t.Fatal("the walk did not terminate")
	}
	if !found[fmt.Sprintf("%d.900", accounts)] {
		t.Fatalf("expected the token on the second service-account page to be synced, got %v", found)
	}

	var searches int
	for _, req := range fixture.recorded() {
		if req.method == http.MethodGet && req.path == "/api/serviceaccounts/search" {
			searches++
		}
	}
	if searches != 2 {
		t.Fatalf("expected exactly two service-account search pages, got %d", searches)
	}
}

// TestListRefusesACapSizedTokenResponse covers the provider's own limit:
// Grafana answers the token list with a SQL LIMIT and no pagination
// (maxRetrievedTokens = 1000), so a response at the cap may be a prefix. A
// truncated list reported as complete would make C1 read the missing tokens as
// deleted, so the sync must fail closed instead.
func TestListRefusesACapSizedTokenResponse(t *testing.T) {
	tokensAt := func(n int) []*grafana.ServiceAccountToken {
		out := make([]*grafana.ServiceAccountToken, 0, n)
		for i := range n {
			out = append(out, &grafana.ServiceAccountToken{ID: int64(1000 + i), Name: fmt.Sprintf("token-%d", i)})
		}
		return out
	}

	for _, tc := range []struct {
		name    string
		count   int
		wantErr bool
	}{
		{name: "one below the cap is a complete list", count: grafanaServiceAccountTokenResponseCap - 1},
		{name: "the cap is refused as possibly truncated", count: grafanaServiceAccountTokenResponseCap, wantErr: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fixture := &tokenFixture{
				serviceAccounts: []*grafana.ServiceAccount{{ID: 7, Name: "sa-seven"}},
				tokens:          map[int][]*grafana.ServiceAccountToken{7: tokensAt(tc.count)},
			}
			builder := newServiceAccountTokenBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)))

			var err error
			pageToken := ""
			for range 5 {
				_, results, listErr := builder.List(context.Background(), nil, syncAttrs(pageToken))
				if listErr != nil {
					err = listErr
					break
				}
				pageToken = nextPageToken(results)
				if pageToken == "" {
					break
				}
			}
			if tc.wantErr {
				if err == nil {
					t.Fatalf("a %d-token response must fail the sync rather than report a complete list", tc.count)
				}
				if !strings.Contains(err.Error(), "per-response cap") {
					t.Fatalf("the failure should name the provider cap, got %v", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("a %d-token response is complete and must sync: %v", tc.count, err)
			}
		})
	}
}

func TestListFailsClosedWhenTokensCannotBeRead(t *testing.T) {
	fixture := &tokenFixture{
		serviceAccounts: []*grafana.ServiceAccount{{ID: 7, Name: "sa-seven"}},
		tokenListStatus: http.StatusForbidden,
	}
	builder := newServiceAccountTokenBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)))

	var err error
	pageToken := ""
	for range 5 {
		_, results, listErr := builder.List(context.Background(), nil, syncAttrs(pageToken))
		if listErr != nil {
			err = listErr
			break
		}
		pageToken = nextPageToken(results)
		if pageToken == "" {
			break
		}
	}
	if err == nil {
		t.Fatal("a denied token list must fail the sync rather than report the account as having no tokens")
	}
	if !strings.Contains(err.Error(), "serviceaccounts:read") {
		t.Fatalf("the failure should name the missing permission, got %v", err)
	}
}

// --- registration gating ---

func TestResourceSyncersGateTokenSupport(t *testing.T) {
	typesFor := func(t *testing.T, syncTokens bool, syncSelection []string) map[string]connectorbuilder.ResourceSyncerV2 {
		t.Helper()
		client := newUncachedCloudClientForTest(t, newTokenFixtureServer(t, &tokenFixture{}))
		g := &Grafana{client: client, SyncServiceAccountTokens: syncTokens}
		if syncSelection != nil {
			g.connectorOpts = connectorOptsWithSyncSelection(syncSelection)
		}
		syncers := map[string]connectorbuilder.ResourceSyncerV2{}
		for _, syncer := range g.ResourceSyncers(context.Background()) {
			syncers[syncer.ResourceType(context.Background()).GetId()] = syncer
		}
		return syncers
	}

	t.Run("no grant means no token type and so no issuer", func(t *testing.T) {
		syncers := typesFor(t, false, nil)
		if _, ok := syncers[resourceTypeServiceAccountToken.Id]; ok {
			t.Fatal("the token type must not be registered without the grant")
		}
	})

	t.Run("the grant registers the token type, its revoke path and the issuer", func(t *testing.T) {
		syncers := typesFor(t, true, nil)
		if _, ok := syncers[resourceTypeServiceAccountToken.Id]; !ok {
			t.Fatal("the grant must register the token type, which carries the revoke path")
		}
		if _, ok := syncers[resourceTypeServiceAccountToken.Id].(connectorbuilder.ResourceActionProvider); !ok {
			t.Fatal("the grant must register the issuer on the token type")
		}
		if _, ok := syncers[resourceTypeServiceAccountToken.Id].(connectorbuilder.ResourceDeleterV2); !ok {
			t.Fatal("the token type that carries the issuer must also be deletable")
		}
		if _, ok := syncers[resourceTypeServiceAccount.Id].(connectorbuilder.ResourceActionProvider); ok {
			t.Fatal("the service account type is the subject, not the secret type, and carries no issuer")
		}
	})

	t.Run("a grant with the type out of the sync selection advertises nothing", func(t *testing.T) {
		syncers := typesFor(t, true, []string{resourceTypeUser.Id})
		if _, ok := syncers[resourceTypeServiceAccountToken.Id]; ok {
			t.Fatal("a type outside the sync selection must not be registered")
		}
	})
}
