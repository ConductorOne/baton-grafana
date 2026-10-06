package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/durationpb"
	"google.golang.org/protobuf/types/known/timestamppb"
)

const (
	// serviceAccountTokenNamePrefix prefixes the provider-side name of every
	// token this connector issues. The rest of the name is the C1 request id,
	// which is what makes a retried issuance detectable instead of duplicable.
	serviceAccountTokenNamePrefix = "c1-"

	// serviceAccountTokenHandleSeparator joins the two provider ids Grafana
	// needs to address one token into the single string the SDK's resource id
	// carries.
	serviceAccountTokenHandleSeparator = "."

	// serviceAccountTokenDetail is the SecretTrait credential detail reported
	// for a Grafana service-account token.
	serviceAccountTokenDetail = "grafana.service_account_token"

	// serviceAccountTokenPlaintextName is the name of the one plaintext value
	// the connector returns for a freshly issued token.
	serviceAccountTokenPlaintextName = "token"

	// serviceAccountTokenDefaultTTL is the lifetime the connector mints a token
	// with when the caller supplies no deadline.
	//
	// Grafana reads secondsToLive == 0 as "never expires", so a missing deadline
	// must never fall through to zero: the connector always sends a positive
	// number of seconds. Until C1 forwards an offering's expiry (IGA-4626) this
	// fallback is the only lifetime a request gets, so it is short on purpose —
	// a day is long enough to retrieve and use a vended credential and short
	// enough that an unrevoked one does not outlive the request that minted it.
	serviceAccountTokenDefaultTTL = 24 * time.Hour

	// serviceAccountTokenMinTTL is the shortest caller-selected lifetime the
	// connector advertises. Grafana accepts any positive secondsToLive, but a
	// floor keeps a request from minting a token that has effectively expired
	// before it is delivered.
	serviceAccountTokenMinTTL = time.Minute

	// maxServiceAccountPages bounds the service-account level of the token walk
	// so a provider that ignores page/perpage and keeps returning full pages
	// fails closed instead of paging forever. 10_000 pages at the connector's
	// page size is far beyond any real Grafana instance's service-account count.
	maxServiceAccountPages = int64(10_000)
)

var (
	_ connectorbuilder.ResourceSyncerV2   = (*serviceAccountTokenBuilder)(nil)
	_ connectorbuilder.ResourceDeleterV2  = (*serviceAccountTokenBuilder)(nil)
	_ connectorbuilder.CredentialIssuerV2 = (*credentialServiceAccountBuilder)(nil)
)

// serviceAccountTokenBuilder syncs Grafana service-account tokens and provides
// the revoke path for the credential issuance that lands on them.
//
// The type is registered only when the operator grants
// sync-service-account-tokens, which is also the only configuration in which
// credential issuance for a service account is advertised. The SDK refuses to
// advertise an issuance option whose secret resource type has no
// ResourceDeleterV2, and this builder is what supplies it.
type serviceAccountTokenBuilder struct {
	client *grafana.Client
}

func newServiceAccountTokenBuilder(client *grafana.Client) *serviceAccountTokenBuilder {
	return &serviceAccountTokenBuilder{client: client}
}

func (t *serviceAccountTokenBuilder) ResourceType(context.Context) *v2.ResourceType {
	return resourceTypeServiceAccountToken
}

func (t *serviceAccountTokenBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	// A token is a secret, not a grant: it inherits its service account's
	// permissions and carries no entitlements of its own.
	return nil, nil, nil
}

func (t *serviceAccountTokenBuilder) Grants(_ context.Context, _ *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// List returns at most one provider page per call. The walk has two levels: the
// paginated service-account search, and then each discovered service account's
// own token list. Both live in the pagination bag — a service-account-level
// state at the bottom and one child state per discovered account pushed above
// it — so the bag is a stack and every call issues exactly one provider
// request. Draining every account's tokens inside one List call would deny the
// SDK any chance to checkpoint, respect rate limits, or cancel.
func (t *serviceAccountTokenBuilder) List(ctx context.Context, _ *v2.ResourceId, attrs rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	bag, page, err := parsePageToken(&attrs.PageToken, &v2.ResourceId{ResourceType: resourceTypeServiceAccountToken.Id})
	if err != nil {
		return nil, nil, fmt.Errorf("grafana-connector: failed to parse page token: %w", err)
	}

	// A child state names the service account whose tokens it is walking; the
	// service-account-level state carries no resource id.
	if current := bag.Current(); current != nil &&
		current.ResourceTypeID == resourceTypeServiceAccount.Id && current.ResourceID != "" {
		return t.listTokensForServiceAccount(ctx, bag, current.ResourceID)
	}

	return t.listServiceAccountsPage(ctx, bag, page)
}

// listServiceAccountsPage consumes one service-account page and pushes a child
// state for every account on it. It returns no resources of its own: the tokens
// are produced by those child states on subsequent calls.
func (t *serviceAccountTokenBuilder) listServiceAccountsPage(ctx context.Context, bag *pagination.Bag, page uint64) ([]*v2.Resource, *rs.SyncOpResults, error) {
	if int64(page) >= maxServiceAccountPages {
		return nil, nil, fmt.Errorf(
			"grafana-connector: exceeded %d service-account pages while syncing service-account tokens",
			maxServiceAccountPages)
	}

	accounts, nextPage, annos, err := t.client.ListServiceAccounts(ctx, &grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	})
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to list service accounts: %w", err)
	}

	if len(accounts) == 0 {
		// The service accounts are exhausted: drop the service-account-level
		// state so the sync ends once the child states pushed by earlier pages
		// are drained.
		bag.Pop()
	} else if err := bag.Next(nextPage); err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to advance service-account page: %w", err)
	}

	for _, account := range accounts {
		if account == nil || account.ID <= 0 {
			continue
		}
		bag.Push(pagination.PageState{
			ResourceTypeID: resourceTypeServiceAccount.Id,
			ResourceID:     strconv.Itoa(account.ID),
		})
	}

	next, err := bag.Marshal()
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to marshal pagination bag: %w", err)
	}

	return nil, &rs.SyncOpResults{NextPageToken: next, Annotations: annos}, nil
}

// listTokensForServiceAccount returns one service account's tokens. Grafana
// answers the whole list in one response, so this child state is consumed in a
// single call.
func (t *serviceAccountTokenBuilder) listTokensForServiceAccount(ctx context.Context, bag *pagination.Bag, serviceAccountID string) ([]*v2.Resource, *rs.SyncOpResults, error) {
	id, err := strconv.Atoi(serviceAccountID)
	if err != nil || id <= 0 {
		return nil, nil, fmt.Errorf("grafana-connector: service account id %q is not a valid Grafana service account id", serviceAccountID)
	}

	tokens, annos, err := t.client.ListServiceAccountTokens(ctx, id)
	if err != nil {
		// Every error fails the sync. Skipping the account instead would report
		// a successful sync that omits its tokens, and C1 reads a resource
		// missing from a completed sync as deleted — which would retire live
		// credentials from the inventory and drop the handles their revocation
		// needs.
		if status.Code(err) == codes.PermissionDenied {
			return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf(
				"grafana-connector: failed to list tokens for service account %q: %w "+
					"(listing service-account tokens requires the Grafana serviceaccounts:read permission)",
				serviceAccountID, err)
		}
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to list tokens for service account %q: %w", serviceAccountID, err)
	}

	parent := &v2.ResourceId{ResourceType: resourceTypeServiceAccount.Id, Resource: serviceAccountID}
	resources := make([]*v2.Resource, 0, len(tokens))
	for _, token := range tokens {
		if token == nil || token.ID <= 0 {
			continue
		}
		resource, err := serviceAccountTokenResource(parent, token)
		if err != nil {
			return nil, &rs.SyncOpResults{Annotations: annos}, err
		}
		resources = append(resources, resource)
	}

	bag.Pop()
	next, err := bag.Marshal()
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to marshal pagination bag: %w", err)
	}

	return resources, &rs.SyncOpResults{NextPageToken: next, Annotations: annos}, nil
}

// Delete removes one service-account token.
//
// Grafana scopes the delete to both the owning service account and the token,
// and neither id is derivable from the other, so both travel in the resource id
// handle (see serviceAccountTokenHandle) rather than relying on the optional
// parent hint, which C1 may or may not send.
//
// A provider that reports the token (or its service account) already gone is
// the outcome the caller asked for: Grafana answers 404 for both, and this
// returns success so a retried or duplicate delete stays idempotent instead of
// becoming a terminal failure.
func (t *serviceAccountTokenBuilder) Delete(ctx context.Context, resourceID *v2.ResourceId, _ *v2.ResourceId) (annotations.Annotations, error) {
	if resourceID == nil || resourceID.GetResource() == "" {
		return nil, status.Error(codes.InvalidArgument, "baton-grafana: a service account token id is required")
	}
	if resourceID.GetResourceType() != resourceTypeServiceAccountToken.Id {
		return nil, status.Errorf(codes.InvalidArgument,
			"baton-grafana: resource type %q is not a Grafana service account token", resourceID.GetResourceType())
	}

	serviceAccountID, tokenID, err := parseServiceAccountTokenHandle(resourceID.GetResource())
	if err != nil {
		return nil, status.Error(codes.InvalidArgument, err.Error())
	}

	if _, err := t.client.DeleteServiceAccountToken(ctx, serviceAccountID, tokenID); err != nil {
		if status.Code(err) == codes.NotFound {
			ctxzap.Extract(ctx).Debug("baton-grafana: service account token is already gone at the provider",
				zap.Int("service_account_id", serviceAccountID),
				zap.Int64("token_id", tokenID))
			return nil, nil
		}
		return nil, fmt.Errorf("baton-grafana: delete service account token: %w", err)
	}

	return nil, nil
}

// credentialServiceAccountBuilder adds credential issuance for an existing
// service account to the service-account syncer. It is registered instead of
// the plain serviceAccountBuilder only when the operator grants
// sync-service-account-tokens, which also registers the token resource type the
// issued credential lands on and the revoke path for it.
type credentialServiceAccountBuilder struct {
	*serviceAccountBuilder
}

func newCredentialServiceAccountBuilder(client *grafana.Client, syncOrgs bool) *credentialServiceAccountBuilder {
	return &credentialServiceAccountBuilder{serviceAccountBuilder: newServiceAccountBuilder(client, syncOrgs)}
}

// IssueCapabilityDetails advertises the one credential kind this connector
// mints: a Grafana service-account token, which is the API_KEY shape.
//
// The descriptor declares an IssuanceExpiryCapability, which is what tells C1
// that Grafana owns this credential's clock: the approved deadline is sent to
// the connector as the token's secondsToLive, and the lifecycle row lands
// CONNECTOR-owned so C1 burns the delivery envelope at expiry instead of
// queueing a provider delete for a credential the provider expires itself.
//
// No scopes are advertised and custom scopes are disallowed: a Grafana service
// account token inherits its service account's permissions and cannot be
// scoped, so the SDK rejects a scoped request for this kind before Issue runs.
func (b *credentialServiceAccountBuilder) IssueCapabilityDetails(context.Context) (*v2.CredentialDetailsCredentialIssue, annotations.Annotations, error) {
	return v2.CredentialDetailsCredentialIssue_builder{
		Options: []*v2.CredentialIssueOptionDescriptor{
			v2.CredentialIssueOptionDescriptor_builder{
				Option: v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY,
				Expiry: v2.IssuanceExpiryCapability_builder{
					Min: durationpb.New(serviceAccountTokenMinTTL),
				}.Build(),
				ResourceMode:         v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE,
				SecretResourceTypeId: resourceTypeServiceAccountToken.Id,
			}.Build(),
		},
		PreferredOption: v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_API_KEY,
	}.Build(), nil, nil
}

// Issue mints one service-account token for an existing service account.
//
// The subject is the synced service account itself: Grafana has no per-request
// service account creation, and a token's permissions are the account's. The
// returned secret carries the provider token id as its revocation handle, and
// the one-time token value as the plaintext the SDK encrypts for delivery.
//
// Issue is not retried automatically. A failure after the provider mint either
// cleans the token up on the spot or reports the failure with the token's id so
// it stays revocable; nothing here re-runs on its own.
func (b *credentialServiceAccountBuilder) Issue(ctx context.Context, input *connectorbuilder.CredentialIssueInput) (*connectorbuilder.CredentialIssueOutput, error) {
	if input == nil || input.IdentityID == nil ||
		input.IdentityID.GetResourceType() != resourceTypeServiceAccount.Id ||
		input.IdentityID.GetResource() == "" {
		return nil, status.Error(codes.InvalidArgument, "baton-grafana: a Grafana service account identity is required")
	}
	if got := input.CredentialOptions.GetSecretResourceTypeId(); got != resourceTypeServiceAccountToken.Id {
		return nil, status.Errorf(codes.InvalidArgument, "baton-grafana: unsupported credential secret resource type %q", got)
	}
	serviceAccountID, err := strconv.Atoi(input.IdentityID.GetResource())
	if err != nil || serviceAccountID <= 0 {
		return nil, status.Errorf(codes.InvalidArgument,
			"baton-grafana: service account id %q is not a valid Grafana service account id", input.IdentityID.GetResource())
	}
	if scopes := input.CredentialOptions.GetApiKey().GetScopes(); len(scopes) != 0 {
		// The SDK rejects a scoped request for this descriptor before Issue runs.
		// Re-checking keeps the arm correct for a caller that reaches Issue
		// directly, and fails closed rather than minting a token whose
		// permissions the caller did not get to choose.
		return nil, status.Error(codes.InvalidArgument,
			"baton-grafana: Grafana service account tokens inherit their service account's permissions and cannot be scoped")
	}

	secondsToLive, expiresAt, err := serviceAccountTokenLifetime(input.ExpiresAt, time.Now().UTC())
	if err != nil {
		return nil, err
	}

	name := serviceAccountTokenName(input.RequestID)

	// Retry safety. The token name is derived from the C1 request id, so a
	// retried issuance finds its predecessor's token instead of minting a
	// second one. Grafana returns the plaintext exactly once, so an existing
	// token cannot be handed back; the retry fails with AlreadyExists and the
	// provider is left holding exactly one credential for the request.
	//
	// This lookup is the fast path, not the guard. The SDK's HTTP client caches
	// GET responses (one hour by default), so a retry that lands inside that
	// window can read a token list taken before the first attempt minted. What
	// actually prevents a duplicate is Grafana itself: token names are unique
	// per organization, so the create below is rejected and mapped to the same
	// AlreadyExists outcome.
	tokens, _, err := b.client.ListServiceAccountTokens(ctx, serviceAccountID)
	if err != nil {
		return nil, fmt.Errorf("baton-grafana: look up service account token for request %q: %w", input.RequestID, err)
	}
	if existing := findServiceAccountTokenByName(tokens, name); existing != nil {
		return nil, duplicateServiceAccountTokenError(name, input.RequestID)
	}

	created, _, err := b.client.CreateServiceAccountToken(ctx, serviceAccountID, name, secondsToLive)
	if err != nil {
		if errors.Is(err, grafana.ErrServiceAccountTokenAlreadyExists) {
			return nil, duplicateServiceAccountTokenError(name, input.RequestID)
		}
		return nil, fmt.Errorf("baton-grafana: create service account token: %w", err)
	}

	if created.ID <= 0 || created.Key == "" {
		// A response without an id has no revocation handle and a response
		// without a value has nothing to deliver. Both leave a live token the
		// caller cannot use or revoke, so remove it before failing.
		b.cleanupIssuedServiceAccountToken(ctx, serviceAccountID, created.ID, name)
		return nil, status.Error(codes.Internal, "baton-grafana: Grafana returned a service account token without an id or value")
	}

	secret, err := issuedServiceAccountTokenResource(input.IdentityID, created.ID, name, expiresAt)
	if err != nil {
		b.cleanupIssuedServiceAccountToken(ctx, serviceAccountID, created.ID, name)
		return nil, fmt.Errorf("baton-grafana: build service account token secret resource: %w", err)
	}

	return &connectorbuilder.CredentialIssueOutput{
		Secret: secret,
		PlaintextData: []*v2.PlaintextData{
			v2.PlaintextData_builder{Name: serviceAccountTokenPlaintextName, Bytes: []byte(created.Key)}.Build(),
		},
		ResourceMode: v2.CredentialResourceMode_CREDENTIAL_RESOURCE_MODE_DISCOVERABLE,
	}, nil
}

// cleanupIssuedServiceAccountToken removes a token this call minted but cannot
// hand back, so a failure after the provider write does not leave a live
// credential with no handle. It is best effort and never changes the caller's
// outcome: a cleanup that fails is logged, because the credential is then live
// and unreferenced and nothing else will retry the removal.
func (b *credentialServiceAccountBuilder) cleanupIssuedServiceAccountToken(ctx context.Context, serviceAccountID int, tokenID int64, name string) {
	l := ctxzap.Extract(ctx)
	// Detached from ctx so cleanup still runs when the caller's context is done.
	cleanupCtx := context.WithoutCancel(ctx)

	if tokenID <= 0 {
		// No id came back, so the name the token was created under is the only
		// handle on it.
		tokens, _, err := b.client.ListServiceAccountTokens(cleanupCtx, serviceAccountID)
		if err != nil {
			l.Error("baton-grafana: failed to look up the service account token to clean up",
				zap.Int("service_account_id", serviceAccountID),
				zap.String("token_name", name),
				zap.Error(err))
			return
		}
		existing := findServiceAccountTokenByName(tokens, name)
		if existing == nil {
			return
		}
		tokenID = existing.ID
	}

	if _, err := b.client.DeleteServiceAccountToken(cleanupCtx, serviceAccountID, tokenID); err != nil && status.Code(err) != codes.NotFound {
		l.Error("baton-grafana: failed to clean up service account token",
			zap.Int("service_account_id", serviceAccountID),
			zap.Int64("token_id", tokenID),
			zap.Error(err))
	}
}

// serviceAccountTokenLifetime resolves the secondsToLive to send to Grafana and
// the expiry to report on the issued credential.
//
// Grafana reads secondsToLive == 0 as "never expires" and a negative value as
// an invalid expiration, so a requested deadline must never be allowed to fall
// through to zero: a missing deadline takes the connector's explicit fallback
// lifetime, and a deadline that has already passed fails the issuance rather
// than minting a token that never expires.
//
// The requested duration is floored to whole seconds, never rounded up, so the
// provider's expiry cannot land later than the deadline C1 recorded. The SDK
// rejects an issued credential whose reported expiry exceeds the requested one.
func serviceAccountTokenLifetime(requested *timestamppb.Timestamp, now time.Time) (int64, time.Time, error) {
	if requested == nil {
		return int64(serviceAccountTokenDefaultTTL / time.Second), now.Add(serviceAccountTokenDefaultTTL), nil
	}
	if err := requested.CheckValid(); err != nil {
		return 0, time.Time{}, status.Errorf(codes.InvalidArgument, "baton-grafana: requested credential expiry is invalid: %v", err)
	}

	minSeconds := int64(serviceAccountTokenMinTTL / time.Second)
	seconds := int64(requested.AsTime().Sub(now) / time.Second)
	if seconds < minSeconds {
		return 0, time.Time{}, status.Errorf(codes.InvalidArgument,
			"baton-grafana: requested credential expiry leaves %d seconds, below the connector's %s minimum",
			seconds, serviceAccountTokenMinTTL)
	}

	return seconds, now.Add(time.Duration(seconds) * time.Second), nil
}

// serviceAccountTokenName is the provider-side name of a token this connector
// issues. It is derived from the C1 request id so a retried request finds its
// predecessor's token instead of minting a second one.
func serviceAccountTokenName(requestID string) string {
	return serviceAccountTokenNamePrefix + requestID
}

func duplicateServiceAccountTokenError(name, requestID string) error {
	return status.Errorf(codes.AlreadyExists,
		"baton-grafana: service account token %q for request %q already exists at the provider; refusing to issue a duplicate",
		name, requestID)
}

func findServiceAccountTokenByName(tokens []*grafana.ServiceAccountToken, name string) *grafana.ServiceAccountToken {
	for _, token := range tokens {
		if token != nil && token.Name == name {
			return token
		}
	}
	return nil
}

// serviceAccountTokenHandle is the credential's revocation handle: the two
// provider ids Grafana needs to delete a token, packed into the resource id C1
// stores and hands back on a delete. Grafana's delete is scoped to both the
// owning service account and the token, and neither is derivable from the
// other, so both travel here.
func serviceAccountTokenHandle(serviceAccountID string, tokenID int64) string {
	return serviceAccountID + serviceAccountTokenHandleSeparator + strconv.FormatInt(tokenID, 10)
}

// parseServiceAccountTokenHandle splits a handle produced by
// serviceAccountTokenHandle. It is strict: both halves must be positive decimal
// ids, because a handle that does not name exactly one provider token cannot be
// turned into a delete that is safe to attempt.
func parseServiceAccountTokenHandle(handle string) (int, int64, error) {
	accountPart, tokenPart, ok := strings.Cut(handle, serviceAccountTokenHandleSeparator)
	if !ok || accountPart == "" || tokenPart == "" {
		return 0, 0, fmt.Errorf(
			"baton-grafana: service account token id %q is not a <service account id>%s<token id> handle",
			handle, serviceAccountTokenHandleSeparator)
	}
	serviceAccountID, err := strconv.Atoi(accountPart)
	if err != nil || serviceAccountID <= 0 {
		return 0, 0, fmt.Errorf("baton-grafana: service account token id %q has an invalid service account id", handle)
	}
	tokenID, err := strconv.ParseInt(tokenPart, 10, 64)
	if err != nil || tokenID <= 0 {
		return 0, 0, fmt.Errorf("baton-grafana: service account token id %q has an invalid token id", handle)
	}
	return serviceAccountID, tokenID, nil
}

// serviceAccountTokenResource builds the synced resource for one service
// account token. Its identity is the owning service account (the same identity
// credential issuance targets), so an issued token and the same token on its
// next sync describe one credential.
func serviceAccountTokenResource(parent *v2.ResourceId, token *grafana.ServiceAccountToken) (*v2.Resource, error) {
	traitOpts := []rs.SecretTraitOption{
		rs.WithSecretIdentityID(parent),
		rs.WithSecretType(v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET),
		rs.WithSecretDetail(serviceAccountTokenDetail),
	}
	if token.Created != nil {
		traitOpts = append(traitOpts, rs.WithSecretCreatedAt(*token.Created))
	}
	if token.LastUsedAt != nil {
		traitOpts = append(traitOpts, rs.WithSecretLastUsedAt(*token.LastUsedAt))
	}
	// Only a token that actually expires carries an expiry. Grafana reports a
	// null expiration for a non-expiring token, and stamping one there would
	// assert a deadline the provider never gave.
	if token.Expiration != nil {
		traitOpts = append(traitOpts, rs.WithSecretExpiresAt(*token.Expiration))
	}

	return rs.NewSecretResource(
		serviceAccountTokenDisplayName(token),
		resourceTypeServiceAccountToken,
		serviceAccountTokenHandle(parent.GetResource(), token.ID),
		traitOpts,
		rs.WithParentResourceID(parent),
	)
}

func serviceAccountTokenDisplayName(token *grafana.ServiceAccountToken) string {
	if token.Name != "" {
		return token.Name
	}
	return fmt.Sprintf("token-%d", token.ID)
}

// issuedServiceAccountTokenResource builds the resource for a token this
// connector just minted. It differs from serviceAccountTokenResource only in
// what the create response reports: the id and the name, plus the expiry the
// connector resolved, since Grafana returns neither created nor last-used on
// create.
func issuedServiceAccountTokenResource(identityID *v2.ResourceId, tokenID int64, name string, expiresAt time.Time) (*v2.Resource, error) {
	return rs.NewSecretResource(
		name,
		resourceTypeServiceAccountToken,
		serviceAccountTokenHandle(identityID.GetResource(), tokenID),
		[]rs.SecretTraitOption{
			rs.WithSecretIdentityID(identityID),
			rs.WithSecretType(v2.SecretTrait_CREDENTIAL_TYPE_STATIC_SECRET),
			rs.WithSecretDetail(serviceAccountTokenDetail),
			rs.WithSecretExpiresAt(expiresAt),
		},
		rs.WithParentResourceID(identityID),
	)
}
