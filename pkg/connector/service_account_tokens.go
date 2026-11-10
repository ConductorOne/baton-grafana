package connector

import (
	"context"
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
)

const (
	// serviceAccountTokenHandleSeparator joins the two provider ids Grafana
	// needs to address one token into the single string the SDK's resource id
	// carries.
	serviceAccountTokenHandleSeparator = "."

	// serviceAccountTokenDetail is the SecretTrait credential detail reported
	// for a Grafana service-account token.
	serviceAccountTokenDetail = "grafana.service_account_token"

	// serviceAccountTokenMinTTL is the shortest caller-selected lifetime the
	// connector advertises. Grafana accepts any positive secondsToLive, but a
	// floor keeps a request from minting a token that has effectively expired
	// before it is delivered.
	serviceAccountTokenMinTTL = time.Minute

	// serviceAccountTokenCleanupTimeout bounds the best-effort removal of a
	// token this connector minted but cannot hand back. It is bounded so a
	// provider that stops answering cannot hold the issuance open, and it is
	// detached from the caller's context so cleanup still runs when that context
	// is already done.
	serviceAccountTokenCleanupTimeout = 30 * time.Second

	// grafanaServiceAccountTokenResponseCap is the most tokens Grafana's token
	// store returns in one response. It is a SQL LIMIT with no pagination
	// (`pkg/services/serviceaccounts/database/token_store.go`, maxRetrievedTokens
	// = 1000, ordered by token name), so a cap-sized response may be truncated.
	//
	// A truncated list reported as complete would make C1 read the missing
	// tokens as deleted and drop their revocation handles, so a cap-sized
	// response fails the sync instead. This is a read refusal, not a quota: the
	// connector does not invent a token limit.
	grafanaServiceAccountTokenResponseCap = 1000

	// maxServiceAccountPages bounds the service-account level of the token walk
	// so a provider that ignores page/perpage and keeps returning full pages
	// fails closed instead of paging forever. 10_000 pages at the connector's
	// page size is far beyond any real Grafana instance's service-account count.
	//
	// uint64 because it is compared against the page number the pagination bag
	// carries, which is uint64: converting the page down to int64 to compare
	// would be a narrowing conversion on a value that can be arbitrarily large.
	maxServiceAccountPages = uint64(10_000)
)

var (
	_ connectorbuilder.ResourceSyncerV2       = (*serviceAccountTokenBuilder)(nil)
	_ connectorbuilder.ResourceDeleterV2      = (*serviceAccountTokenBuilder)(nil)
	_ connectorbuilder.ResourceActionProvider = (*serviceAccountTokenBuilder)(nil)
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
	if page >= maxServiceAccountPages {
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

	if len(tokens) >= grafanaServiceAccountTokenResponseCap {
		// The provider answered with exactly its per-response cap. There is no
		// pagination to walk, so this list may be a prefix of the account's
		// tokens; reporting it as complete would let C1 treat the missing ones as
		// deleted. Fail closed and say which limit was hit.
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf(
			"grafana-connector: service account %q returned %d tokens, the provider's per-response cap; "+
				"the list cannot be reported as complete because Grafana caps this endpoint with a SQL LIMIT and no pagination",
			serviceAccountID, len(tokens))
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
	if token.LastUsedAt != nil {
		traitOpts = append(traitOpts, rs.WithSecretLastUsedAt(*token.LastUsedAt))
	}
	// Only a token that actually expires carries an expiry. Grafana reports a
	// null expiration for a non-expiring token, and stamping one there would
	// assert a deadline the provider never gave.
	if token.Expiration != nil {
		traitOpts = append(traitOpts, rs.WithSecretExpiresAt(*token.Expiration))
	}

	// created_at lives on the resource, not the trait: WithSecretCreatedAt is
	// deprecated in favour of WithResourceCreatedAt, which sets the same field
	// the SDK populates for a resource built with NewSecretResource.
	resourceOpts := []rs.ResourceOption{rs.WithParentResourceID(parent)}
	if token.Created != nil {
		resourceOpts = append(resourceOpts, rs.WithResourceCreatedAt(*token.Created))
	}

	return rs.NewSecretResource(
		serviceAccountTokenDisplayName(token),
		resourceTypeServiceAccountToken,
		serviceAccountTokenHandle(parent.GetResource(), token.ID),
		traitOpts,
		resourceOpts...,
	)
}

func serviceAccountTokenDisplayName(token *grafana.ServiceAccountToken) string {
	if token.Name != "" {
		return token.Name
	}
	return fmt.Sprintf("token-%d", token.ID)
}
