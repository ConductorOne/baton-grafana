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
	"google.golang.org/protobuf/types/known/timestamppb"
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
		created2 = &grafana.CreatedServiceAccountToken{ID: 41, Name: "c1-ticket-1", Key: "glsa_one_time_value"}
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
		created = &grafana.CreatedServiceAccountToken{ID: 41, Name: "c1-ticket-1", Key: "glsa_one_time_value"}
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

func tokenTestInput(serviceAccountID, requestID string) *connectorbuilder.CredentialIssueInput {
	return &connectorbuilder.CredentialIssueInput{
		IdentityID: &v2.ResourceId{ResourceType: resourceTypeServiceAccount.Id, Resource: serviceAccountID},
		CredentialOptions: v2.CredentialIssueOptions_builder{
			SecretResourceTypeId: resourceTypeServiceAccountToken.Id,
			ApiKey:               v2.CredentialIssueOptions_ApiKey_builder{}.Build(),
		}.Build(),
		RequestID: requestID,
	}
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

// --- lifetime resolution ---

func TestServiceAccountTokenLifetime(t *testing.T) {
	now := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC)

	t.Run("a missing deadline takes the explicit fallback, never zero", func(t *testing.T) {
		seconds, deadline, err := serviceAccountTokenLifetime(nil, now)
		if err != nil {
			t.Fatalf("lifetime: %v", err)
		}
		if seconds != int64(serviceAccountTokenDefaultTTL/time.Second) {
			t.Fatalf("expected the fallback lifetime %d, got %d", int64(serviceAccountTokenDefaultTTL/time.Second), seconds)
		}
		if seconds == 0 {
			t.Fatal("a missing deadline must never become secondsToLive=0, which Grafana reads as never expires")
		}
		if deadline != nil {
			t.Fatalf("the connector chose this lifetime, so there is no approved deadline to verify against, got %s", deadline)
		}
	})

	t.Run("a requested deadline is forwarded less the dispatch buffer, floored", func(t *testing.T) {
		requested := now.Add(2*time.Hour + 900*time.Millisecond)
		seconds, deadline, err := serviceAccountTokenLifetime(timestamppb.New(requested), now)
		if err != nil {
			t.Fatalf("lifetime: %v", err)
		}
		want := int64((2*time.Hour + 900*time.Millisecond - serviceAccountTokenClockSkewBuffer) / time.Second)
		if seconds != want {
			t.Fatalf("expected %d seconds, got %d", want, seconds)
		}
		if seconds >= int64(2*time.Hour/time.Second) {
			t.Fatal("the sent lifetime must leave room for the round trip, or the provider's expiry can land past the deadline")
		}
		if deadline == nil || !deadline.Equal(requested) {
			t.Fatalf("expected the approved deadline %s, got %v", requested, deadline)
		}
	})

	t.Run("a deadline below the connector minimum fails instead of minting", func(t *testing.T) {
		requested := now.Add(serviceAccountTokenMinTTL - time.Second)
		seconds, _, err := serviceAccountTokenLifetime(timestamppb.New(requested), now)
		if err == nil {
			t.Fatal("expected an error for a deadline below the minimum")
		}
		if status.Code(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument, got %v", err)
		}
		if seconds != 0 {
			t.Fatalf("a rejected deadline must not yield a lifetime, got %d", seconds)
		}
	})

	t.Run("a deadline in the past fails", func(t *testing.T) {
		if _, _, err := serviceAccountTokenLifetime(timestamppb.New(now.Add(-time.Minute)), now); err == nil {
			t.Fatal("expected an error for a past deadline")
		}
	})

	t.Run("an invalid timestamp fails", func(t *testing.T) {
		if _, _, err := serviceAccountTokenLifetime(&timestamppb.Timestamp{Seconds: -1, Nanos: -1}, now); err == nil {
			t.Fatal("expected an error for an invalid timestamp")
		}
	})
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

// --- capabilities ---

func TestIssueCapabilityDetailsAdvertiseProviderExpiry(t *testing.T) {
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, &tokenFixture{})), false)

	details, _, err := builder.IssueCapabilityDetails(context.Background())
	if err != nil {
		t.Fatalf("IssueCapabilityDetails: %v", err)
	}
	if len(details.GetOptions()) != 1 {
		t.Fatalf("expected exactly one advertised option, got %d", len(details.GetOptions()))
	}
	descriptor := details.GetOptions()[0]
	if descriptor.GetOption() != v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY {
		t.Fatalf("expected the API_KEY shape, got %s", descriptor.GetOption())
	}
	if descriptor.GetSecretResourceTypeId() != resourceTypeServiceAccountToken.Id {
		t.Fatalf("expected secret resource type %q, got %q", resourceTypeServiceAccountToken.Id, descriptor.GetSecretResourceTypeId())
	}
	if descriptor.GetResourceMode() != v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE {
		t.Fatalf("a synced token is discoverable, got %s", descriptor.GetResourceMode())
	}
	// The expiry capability is what makes C1 treat Grafana as the owner of this
	// credential's clock and forward the approved deadline instead of queueing a
	// provider delete at its own expiry.
	if descriptor.GetExpiry() == nil {
		t.Fatal("the descriptor must declare an issuance expiry capability")
	}
	if descriptor.GetExpiry().GetMin().AsDuration() != serviceAccountTokenMinTTL {
		t.Fatalf("expected minimum %s, got %s", serviceAccountTokenMinTTL, descriptor.GetExpiry().GetMin().AsDuration())
	}
	if descriptor.GetExpiry().GetMax() != nil {
		t.Fatal("Grafana's token lifetime ceiling is instance-configured and not readable, so no maximum is advertised")
	}
	if len(descriptor.GetScopes()) != 0 || descriptor.GetCustomScopesAllowed() {
		t.Fatal("a Grafana service account token cannot be scoped, so the descriptor must not advertise scopes")
	}
	if details.GetPreferredOption() != v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY {
		t.Fatalf("unexpected preferred option %s", details.GetPreferredOption())
	}
}

// TestCapabilitiesAdvertiseTokenIssuance runs the connector through the SDK's
// own capability builder, which is what validates that every advertised
// issuance option has a registered revoke path for the resource type it names.
func TestCapabilitiesAdvertiseTokenIssuance(t *testing.T) {
	fixture := &tokenFixture{}
	g := &Grafana{
		client:                   newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)),
		SyncServiceAccountTokens: true,
	}

	server, err := connectorbuilder.NewConnector(context.Background(), g)
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	md, err := server.GetMetadata(context.Background(), &v2.ConnectorServiceGetMetadataRequest{})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}

	var found bool
	for _, rtc := range md.GetMetadata().GetCapabilities().GetResourceTypeCapabilities() {
		if rtc.GetResourceType().GetId() != resourceTypeServiceAccount.Id {
			continue
		}
		for _, descriptor := range rtc.GetCredentialIssue().GetOptions() {
			if descriptor.GetSecretResourceTypeId() == resourceTypeServiceAccountToken.Id {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("the service account type must advertise the service account token issuance option")
	}

	var tokenTypeDeclared bool
	for _, rtc := range md.GetMetadata().GetCapabilities().GetResourceTypeCapabilities() {
		if rtc.GetResourceType().GetId() == resourceTypeServiceAccountToken.Id {
			tokenTypeDeclared = true
			if !rtc.GetOptInRequired() {
				t.Fatal("the token type must be opt-in: it needs serviceaccounts:write and is not wanted by every tenant")
			}
		}
	}
	if !tokenTypeDeclared {
		t.Fatal("the token resource type must be declared, or C1 cannot resolve the credential's landing type")
	}
}

func TestCapabilitiesOmitTokenIssuanceWithoutTheGrant(t *testing.T) {
	fixture := &tokenFixture{}
	g := &Grafana{client: newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture))}

	server, err := connectorbuilder.NewConnector(context.Background(), g)
	if err != nil {
		t.Fatalf("NewConnector: %v", err)
	}
	md, err := server.GetMetadata(context.Background(), &v2.ConnectorServiceGetMetadataRequest{})
	if err != nil {
		t.Fatalf("GetMetadata: %v", err)
	}

	for _, rtc := range md.GetMetadata().GetCapabilities().GetResourceTypeCapabilities() {
		if rtc.GetResourceType().GetId() == resourceTypeServiceAccountToken.Id {
			t.Fatal("without the grant the token type must not be registered")
		}
		if rtc.GetCredentialIssue() != nil {
			t.Fatalf("without the grant %q must not advertise credential issuance", rtc.GetResourceType().GetId())
		}
	}
}

// --- Issue ---

func TestIssueReportsTheProvidersOwnExpiry(t *testing.T) {
	providerExpiry := time.Now().UTC().Add(90 * time.Minute).Truncate(time.Second)
	fixture := &tokenFixture{createdExpiration: &providerExpiry}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	requested := time.Now().UTC().Add(3 * time.Hour)
	input := tokenTestInput("7", "ticket-1")
	input.ExpiresAt = timestamppb.New(requested)

	out, err := builder.Issue(context.Background(), input)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	requests := fixture.recorded()
	if len(requests) != 3 {
		t.Fatalf("expected a pre-check list, a create and a readback, got %d requests: %+v", len(requests), requests)
	}
	create := requests[1]
	if create.method != http.MethodPost || create.path != "/api/serviceaccounts/7/tokens" {
		t.Fatalf("unexpected create request %s %s", create.method, create.path)
	}
	if got := create.body["name"]; got != "c1-ticket-1" {
		t.Fatalf("expected the deterministic name c1-ticket-1, got %v", got)
	}
	secondsToLive, ok := create.body["secondsToLive"].(float64)
	if !ok {
		t.Fatalf("secondsToLive must be sent, got %v", create.body["secondsToLive"])
	}
	if secondsToLive <= 0 {
		t.Fatalf("secondsToLive must be positive, got %v", secondsToLive)
	}
	// The sent lifetime leaves room for the round trip, so the provider's own
	// expiry can still land inside the approved deadline.
	remaining := requested.Sub(time.Now().UTC()).Seconds()
	bufferSeconds := serviceAccountTokenClockSkewBuffer.Seconds()
	if secondsToLive > remaining-bufferSeconds {
		t.Fatalf("expected the dispatch buffer to be removed from %v seconds, got %v", remaining, secondsToLive)
	}
	if requests[2].method != http.MethodGet {
		t.Fatalf("expected the create to be followed by an uncached readback, got %s", requests[2].method)
	}

	if out.Secret.GetId().GetResourceType() != resourceTypeServiceAccountToken.Id {
		t.Fatalf("unexpected secret resource type %q", out.Secret.GetId().GetResourceType())
	}
	if out.Secret.GetId().GetResource() != "7.41" {
		t.Fatalf("expected the packed revocation handle 7.41, got %q", out.Secret.GetId().GetResource())
	}
	if out.Secret.GetParentResourceId().GetResource() != "7" ||
		out.Secret.GetParentResourceId().GetResourceType() != resourceTypeServiceAccount.Id {
		t.Fatalf("unexpected parent %v", out.Secret.GetParentResourceId())
	}
	if out.ResourceMode != v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE {
		t.Fatalf("unexpected resource mode %s", out.ResourceMode)
	}

	trait := secretTraitOf(t, out.Secret)
	if trait.GetCredentialDetail() != serviceAccountTokenDetail {
		t.Fatalf("expected credential detail %q, got %q", serviceAccountTokenDetail, trait.GetCredentialDetail())
	}
	if trait.GetIdentityId().GetResource() != "7" {
		t.Fatalf("the trait identity must be the service account, got %v", trait.GetIdentityId())
	}
	// The reported expiry must be the provider's, not a locally derived value.
	if trait.GetExpiresAt() == nil {
		t.Fatal("an issued token always expires, so the trait must carry an expiry")
	}
	if !trait.GetExpiresAt().AsTime().Equal(providerExpiry) {
		t.Fatalf("expected the provider's own expiry %s, got %s", providerExpiry, trait.GetExpiresAt().AsTime())
	}
	if trait.GetExpiresAt().AsTime().After(requested) {
		t.Fatalf("reported expiry %s must not exceed the requested %s", trait.GetExpiresAt().AsTime(), requested)
	}

	if len(out.PlaintextData) != 1 {
		t.Fatalf("expected one plaintext value, got %d", len(out.PlaintextData))
	}
	if out.PlaintextData[0].GetName() != serviceAccountTokenPlaintextName {
		t.Fatalf("unexpected plaintext name %q", out.PlaintextData[0].GetName())
	}
	if string(out.PlaintextData[0].GetBytes()) != "glsa_one_time_value" {
		t.Fatalf("unexpected plaintext value %q", out.PlaintextData[0].GetBytes())
	}
}

func TestIssueWithoutDeadlineMintsWithFallbackTTL(t *testing.T) {
	providerExpiry := time.Now().UTC().Add(24 * time.Hour)
	fixture := &tokenFixture{createdExpiration: &providerExpiry}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	out, err := builder.Issue(context.Background(), tokenTestInput("7", "ticket-1"))
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}

	create := fixture.recorded()[1]
	secondsToLive, ok := create.body["secondsToLive"].(float64)
	if !ok {
		t.Fatalf("secondsToLive must be sent, got %v", create.body["secondsToLive"])
	}
	if secondsToLive != float64(serviceAccountTokenDefaultTTL/time.Second) {
		t.Fatalf("expected the fallback lifetime %d, got %v", int64(serviceAccountTokenDefaultTTL/time.Second), secondsToLive)
	}
	if secondsToLive == 0 {
		t.Fatal("a request without a deadline must never mint a non-expiring token")
	}

	trait := secretTraitOf(t, out.Secret)
	if trait.GetExpiresAt() == nil {
		t.Fatal("the issued credential must report the expiry the provider gave it")
	}
}

// TestIssueRejectsAProviderExpiryPastTheApprovedDeadline covers the case the
// dispatch buffer cannot rule out: the provider handled the create late enough
// that its own expiry landed after the deadline C1 approved. The connector must
// not clamp the value it reports, and must not hand back a credential that
// outlives the request.
func TestIssueRejectsAProviderExpiryPastTheApprovedDeadline(t *testing.T) {
	requested := time.Now().UTC().Add(2 * time.Hour).Truncate(time.Second)
	lateExpiry := requested.Add(time.Second)
	fixture := &tokenFixture{createdExpiration: &lateExpiry}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	input := tokenTestInput("7", "ticket-1")
	input.ExpiresAt = timestamppb.New(requested)

	out, err := builder.Issue(context.Background(), input)
	if err == nil {
		t.Fatalf("expected the issuance to fail, got %v", out)
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
	if !strings.Contains(err.Error(), "after the approved deadline") {
		t.Fatalf("the failure should name the deadline breach, got %v", err)
	}
	if n := fixture.countMethodPath(http.MethodDelete, "/api/serviceaccounts/7/tokens/41"); n != 1 {
		t.Fatalf("expected the over-long token to be removed once, got %d deletes: %+v", n, fixture.recorded())
	}
}

func TestIssueRejectsANonExpiringTokenFromTheProvider(t *testing.T) {
	fixture := &tokenFixture{createdExpiration: nil}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	input := tokenTestInput("7", "ticket-1")
	input.ExpiresAt = timestamppb.New(time.Now().UTC().Add(time.Hour))

	if _, err := builder.Issue(context.Background(), input); err == nil {
		t.Fatal("expected a provider-reported non-expiring token to fail the issuance")
	}
	if n := fixture.countMethodPath(http.MethodDelete, "/api/serviceaccounts/7/tokens/41"); n != 1 {
		t.Fatalf("expected the non-expiring token to be removed once, got %d deletes: %+v", n, fixture.recorded())
	}
}

func TestIssueRejectsWhenTheProviderDoesNotReportTheTokenItMinted(t *testing.T) {
	// The readback answers with a token list that does not contain the created
	// token, so the connector cannot verify anything about it.
	fixture := &tokenFixture{tokensAfterCreate: map[int][]*grafana.ServiceAccountToken{7: {}}}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	if _, err := builder.Issue(context.Background(), tokenTestInput("7", "ticket-1")); err == nil {
		t.Fatal("expected the issuance to fail when the created token cannot be read back")
	}
	if n := fixture.countMethodPath(http.MethodDelete, "/api/serviceaccounts/7/tokens/41"); n != 1 {
		t.Fatalf("expected the unverifiable token to be removed once, got %d deletes: %+v", n, fixture.recorded())
	}
}

func TestIssueRejectsAnUndecodableProviderExpiry(t *testing.T) {
	fixture := &tokenFixture{tokenListBodyAfterCreate: `[{"id":41,"name":"c1-ticket-1","expiration":"not-a-timestamp"}]`}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	if _, err := builder.Issue(context.Background(), tokenTestInput("7", "ticket-1")); err == nil {
		t.Fatal("expected a malformed provider expiry to fail the issuance")
	}
	if n := fixture.countMethodPath(http.MethodDelete, "/api/serviceaccounts/7/tokens/41"); n != 1 {
		t.Fatalf("expected the unverifiable token to be removed once, got %d deletes: %+v", n, fixture.recorded())
	}
}

func TestIssueSurfacesAFailedCleanup(t *testing.T) {
	fixture := &tokenFixture{
		createdExpiration: nil,
		deleteStatus:      http.StatusInternalServerError,
	}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	input := tokenTestInput("7", "ticket-1")
	input.ExpiresAt = timestamppb.New(time.Now().UTC().Add(time.Hour))

	_, err := builder.Issue(context.Background(), input)
	if err == nil {
		t.Fatal("expected an error")
	}
	if !strings.Contains(err.Error(), "may still be live") {
		t.Fatalf("a cleanup that did not complete must be part of the returned error, got %v", err)
	}
	if !strings.Contains(err.Error(), "non-expiring") {
		t.Fatalf("the rejection reason must survive alongside the cleanup failure, got %v", err)
	}
}

func TestIssueRefusesToMintADuplicateForTheSameRequest(t *testing.T) {
	fixture := &tokenFixture{tokens: map[int][]*grafana.ServiceAccountToken{
		7: {{ID: 41, Name: "c1-ticket-1"}},
	}}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	_, err := builder.Issue(context.Background(), tokenTestInput("7", "ticket-1"))
	if err == nil {
		t.Fatal("expected the retry to fail rather than mint a second token")
	}
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}
	if n := fixture.countMethod(http.MethodPost); n != 0 {
		t.Fatalf("a duplicate must be detected before the provider is asked to mint, got %d creates", n)
	}
}

// TestIssueReliesOnProviderUniquenessWhenThePrecheckIsStale covers the race the
// contract records as unresolvable by a list-before-create: the pre-check sees
// no token (a stale cached read, or a concurrent create), and the provider's own
// name uniqueness is what stops the duplicate. It must be classified as a
// duplicate, and the existing credential must not be revoked.
func TestIssueReliesOnProviderUniquenessWhenThePrecheckIsStale(t *testing.T) {
	fixture := &tokenFixture{
		createStatus: http.StatusBadRequest,
		createBody: map[string]any{
			"message": "service account token with given name already exists in the organization",
		},
	}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	_, err := builder.Issue(context.Background(), tokenTestInput("7", "ticket-1"))
	if status.Code(err) != codes.AlreadyExists {
		t.Fatalf("expected AlreadyExists, got %v", err)
	}
	if n := fixture.countMethod(http.MethodPost); n != 1 {
		t.Fatalf("expected exactly one create attempt, got %d", n)
	}
	if n := fixture.countMethod(http.MethodDelete); n != 0 {
		t.Fatalf("a duplicate must never revoke the credential that already exists, got %d deletes", n)
	}
}

// TestIssueClassifiesOnlyARealNameConflictAsDuplicate guards the narrow
// classification: a 400 for any other reason must not be reported as a
// duplicate, because that would tell the caller a credential exists when none
// does.
func TestIssueClassifiesOnlyARealNameConflictAsDuplicate(t *testing.T) {
	fixture := &tokenFixture{
		createStatus: http.StatusBadRequest,
		createBody:   map[string]any{"message": "Number of seconds before expiration is greater than the global limit"},
	}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	input := tokenTestInput("7", "ticket-1")
	input.ExpiresAt = timestamppb.New(time.Now().UTC().Add(time.Hour))

	_, err := builder.Issue(context.Background(), input)
	if err == nil {
		t.Fatal("a provider rejection must fail the issuance")
	}
	if status.Code(err) == codes.AlreadyExists {
		t.Fatalf("a lifetime rejection is not a name conflict, got %v", err)
	}
	if !strings.Contains(err.Error(), "global limit") {
		t.Fatalf("the provider's reason should survive, got %v", err)
	}
}

// TestConcurrentIssuanceLeavesExactlyOneCredential drives two issuances for the
// same request at the same time, both released only after both have reached the
// provider's create. The pre-check is therefore stale for both, which is exactly
// the race a list-before-create cannot close.
//
// What keeps it safe is the provider's own storage: the api_key table carries a
// real UNIQUE (org_id, name) index
// (pkg/services/sqlstore/migrations/apikey_mig.go), so the second INSERT cannot
// commit. The fixture models that, and the assertions are that exactly one
// credential is minted and that the loser never revokes the winner's.
func TestConcurrentIssuanceLeavesExactlyOneCredential(t *testing.T) {
	providerExpiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	fixture := &tokenFixture{
		mintedNames:       map[string]bool{},
		arrived:           make(chan struct{}, 2),
		release:           make(chan struct{}),
		createdExpiration: &providerExpiry,
	}
	// Deliberately the caching client, not the uncached one: two concurrent
	// requests through a client whose cache is the SDK's noop cache race on that
	// cache's unsynchronized counter (uhttp.(*NoopCache).Get), which is an SDK
	// defect this test would otherwise report as its own. The memory cache is
	// synchronized, and a shared empty pre-check is exactly the race under test.
	builder := newCredentialServiceAccountBuilder(newCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	type result struct {
		out *connectorbuilder.CredentialIssueOutput
		err error
	}
	results := make(chan result, 2)
	for range 2 {
		go func() {
			out, err := builder.Issue(context.Background(), tokenTestInput("7", "ticket-1"))
			results <- result{out: out, err: err}
		}()
	}

	// Hold the creates until both have arrived, so neither issuance can win by
	// being scheduled first. Bounded, because one issuance may legitimately be
	// caught by its pre-check before it reaches the provider at all.
	go func() {
		for range 2 {
			select {
			case <-fixture.arrived:
			case <-time.After(500 * time.Millisecond):
			}
		}
		close(fixture.release)
	}()

	// Exactly one issuance may succeed, whichever way the race falls: the loser
	// is caught either by its own pre-check (the provider's named conflict) or
	// by the store's unique index (the raw insert failure). Both are failures,
	// and neither may take the winner's credential with it.
	var succeeded, failed int
	for range 2 {
		res := <-results
		switch {
		case res.err == nil:
			succeeded++
			if res.out.Secret.GetId().GetResource() != "7.41" {
				t.Fatalf("unexpected handle %q", res.out.Secret.GetId().GetResource())
			}
		default:
			failed++
			if status.Code(res.err) == codes.AlreadyExists {
				// The loser's own pre-check saw the winner's token: the fast path
				// worked and the provider was never asked to mint a duplicate.
				continue
			}
			// Otherwise the store's unique index rejected the INSERT, which the
			// API renders as its raw insert failure rather than the named
			// conflict. The connector does not reclassify that as a duplicate --
			// the text is database-specific -- so the caller sees the provider's
			// failure, which is what this asserts.
			if !strings.Contains(res.err.Error(), "failed to add service account token") {
				t.Fatalf("expected the provider's named name conflict or its raw insert failure, got %v", res.err)
			}
		}
	}
	if succeeded != 1 || failed != 1 {
		t.Fatalf("expected exactly one issuance to succeed, got %d succeeded and %d failed", succeeded, failed)
	}

	fixture.mu.Lock()
	minted := len(fixture.mintedNames)
	fixture.mu.Unlock()
	if minted != 1 {
		t.Fatalf("expected exactly one provider credential to be minted, got %d", minted)
	}
	if n := fixture.countMethod(http.MethodDelete); n != 0 {
		t.Fatalf("the losing issuance must never revoke the winner's credential, got %d deletes: %+v", n, fixture.recorded())
	}
}

// TestIssueReadBackBypassesTheResponseCache leaves the SDK's HTTP response cache
// on: the pre-check's body is cached, and the readback must still observe the
// token the create added. Without the no-cache readback the second list would be
// served from the cache and the connector would report a failure.
func TestIssueReadBackBypassesTheResponseCache(t *testing.T) {
	providerExpiry := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	fixture := &tokenFixture{createdExpiration: &providerExpiry}
	// Deliberately the caching client, not newUncachedCloudClientForTest.
	builder := newCredentialServiceAccountBuilder(newCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	input := tokenTestInput("7", "ticket-1")
	input.ExpiresAt = timestamppb.New(time.Now().UTC().Add(2 * time.Hour))

	out, err := builder.Issue(context.Background(), input)
	if err != nil {
		t.Fatalf("Issue: %v", err)
	}
	if got := secretTraitOf(t, out.Secret).GetExpiresAt().AsTime(); !got.Equal(providerExpiry) {
		t.Fatalf("expected the provider expiry %s, got %s", providerExpiry, got)
	}
	if n := fixture.countMethodPath(http.MethodGet, "/api/serviceaccounts/7/tokens"); n < 2 {
		t.Fatalf("the readback must reach the provider, got %d token lists", n)
	}
}

func TestIssueCleansUpATokenItCannotHandBack(t *testing.T) {
	fixture := &tokenFixture{
		// Grafana answered, but without an id there is no revocation handle and
		// without a value there is nothing to deliver.
		createResponse: &grafana.CreatedServiceAccountToken{Name: "c1-ticket-1"},
		// The token exists at the provider only after the mint, which is what
		// forces the cleanup to resolve it by name.
		tokensAfterCreate: map[int][]*grafana.ServiceAccountToken{
			7: {{ID: 41, Name: "c1-ticket-1"}},
		},
	}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	_, err := builder.Issue(context.Background(), tokenTestInput("7", "ticket-1"))
	if err == nil {
		t.Fatal("expected an error when the provider returns no id or value")
	}
	if status.Code(err) != codes.Internal {
		t.Fatalf("expected Internal, got %v", err)
	}
	if n := fixture.countMethodPath(http.MethodDelete, "/api/serviceaccounts/7/tokens/41"); n != 1 {
		t.Fatalf("expected the unusable token to be removed once, got %d deletes: %+v", n, fixture.recorded())
	}
}

func TestIssueRejectsScopedRequests(t *testing.T) {
	fixture := &tokenFixture{}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	input := tokenTestInput("7", "ticket-1")
	input.CredentialOptions = v2.CredentialIssueOptions_builder{
		SecretResourceTypeId: resourceTypeServiceAccountToken.Id,
		ApiKey:               v2.CredentialIssueOptions_ApiKey_builder{Scopes: []string{"metrics:read"}}.Build(),
	}.Build()

	if _, err := builder.Issue(context.Background(), input); status.Code(err) != codes.InvalidArgument {
		t.Fatalf("expected InvalidArgument, got %v", err)
	}
	if n := fixture.countMethod(http.MethodPost); n != 0 {
		t.Fatalf("a rejected request must not reach the provider, got %d creates", n)
	}
}

func TestIssueRequiresAServiceAccountIdentity(t *testing.T) {
	fixture := &tokenFixture{}
	builder := newCredentialServiceAccountBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)), false)

	for _, input := range []*connectorbuilder.CredentialIssueInput{
		nil,
		tokenTestInput("not-a-number", "ticket-1"),
		{IdentityID: &v2.ResourceId{ResourceType: resourceTypeUser.Id, Resource: "3"}, RequestID: "ticket-1"},
	} {
		if _, err := builder.Issue(context.Background(), input); status.Code(err) != codes.InvalidArgument {
			t.Fatalf("expected InvalidArgument for %v, got %v", input, err)
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

	t.Run("no grant means no token type and no issuance", func(t *testing.T) {
		syncers := typesFor(t, false, nil)
		if _, ok := syncers[resourceTypeServiceAccountToken.Id]; ok {
			t.Fatal("the token type must not be registered without the grant")
		}
		if _, ok := syncers[resourceTypeServiceAccount.Id].(*credentialServiceAccountBuilder); ok {
			t.Fatal("issuance must not be advertised without the grant")
		}
	})

	t.Run("the grant registers both the token type and issuance", func(t *testing.T) {
		syncers := typesFor(t, true, nil)
		if _, ok := syncers[resourceTypeServiceAccountToken.Id]; !ok {
			t.Fatal("the grant must register the token type, which carries the revoke path")
		}
		if _, ok := syncers[resourceTypeServiceAccount.Id].(*credentialServiceAccountBuilder); !ok {
			t.Fatal("the grant must advertise issuance for the service account type")
		}
	})

	t.Run("a grant with the type out of the sync selection advertises nothing", func(t *testing.T) {
		syncers := typesFor(t, true, []string{resourceTypeUser.Id})
		if _, ok := syncers[resourceTypeServiceAccountToken.Id]; ok {
			t.Fatal("a type outside the sync selection must not be registered")
		}
		if _, ok := syncers[resourceTypeServiceAccount.Id].(*credentialServiceAccountBuilder); ok {
			t.Fatal("issuance must not be advertised when the credential's landing type is not synced")
		}
	})
}
