package connector

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"time"

	configv1 "github.com/conductorone/baton-sdk/pb/c1/config/v1"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/actions"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	"github.com/conductorone/baton-multipass-sdk/pkg/multipass"
)

// The service-account token type carries one issuer: a resource-scoped
// ACTION_TYPE_RESOURCE_CREATE action that mints a token for a Grafana service
// account. C1 recognizes it as a credential issuer by its schema alone: the
// action type, the one secret return field whose content type is a Multipass
// type, and the resource return that carries the created token. Revoking an
// issued token is the resource delete the token syncer already provides.

const (
	// ActionIssueServiceAccountToken is the name of the issuer action.
	ActionIssueServiceAccountToken = "issue_service_account_token"

	// actionArgServiceAccountID is the one subject: the Grafana service account
	// that owns the token.
	actionArgServiceAccountID = "service_account_id"
	// actionArgSecondsToLive is the admin-fixed token lifetime.
	actionArgSecondsToLive = "seconds_to_live"

	actionReturnToken    = "token"
	actionReturnResource = "resource"

	// minSecondsToLive is the shortest lifetime an offering can fix. Grafana
	// reads secondsToLive == 0 as "never expires", and a token that has
	// effectively expired before it is delivered is useless, so the floor is
	// also what keeps the connector from ever sending zero.
	minSecondsToLive = int64(serviceAccountTokenMinTTL / time.Second)

	grafanaProviderName = "grafana"
	// grafanaTokenHeaderName is the HTTP header a service-account token is
	// presented in: `Authorization: Bearer <token>`.
	grafanaTokenHeaderName = "Authorization"

	// issuedTokenNameSuffixLen is the length of the random suffix of an
	// issued token name: 6 base32 characters, 30 bits.
	issuedTokenNameSuffixLen = 6
)

// serviceAccountTokenActionSchema is the issuer's schema. The service account
// is the one subject, constrained to the service account type. The lifetime is
// an admin-fixed offering value: Grafana mints a token that never expires when
// no lifetime is sent, so the connector requires one instead of choosing a
// default for the admin.
func serviceAccountTokenActionSchema() *v2.BatonActionSchema {
	secret := multipass.OutputField(actionReturnToken)
	secret.SetIsRequired(true)
	minTTL := minSecondsToLive
	return v2.BatonActionSchema_builder{
		Name:        ActionIssueServiceAccountToken,
		DisplayName: "Issue Grafana service account token",
		Description: "Creates a token for a Grafana service account and returns it as a Multipass api_key_v2 secret.",
		Arguments: []*configv1.Field{
			configv1.Field_builder{
				Name:        actionArgServiceAccountID,
				DisplayName: "Service account",
				Description: "The Grafana service account that owns the token. The token has that account's permissions.",
				IsRequired:  true,
				ResourceIdField: configv1.ResourceIdField_builder{
					Rules: configv1.ResourceIDRules_builder{
						AllowedResourceTypeIds: []string{resourceTypeServiceAccount.Id},
					}.Build(),
				}.Build(),
			}.Build(),
			configv1.Field_builder{
				Name:        actionArgSecondsToLive,
				DisplayName: "Token lifetime (seconds)",
				Description: "How long Grafana keeps the token valid. Grafana backstops the revocation C1 performs when the offering expires, so set it no shorter than the offering's duration.",
				IsRequired:  true,
				IntField: configv1.IntField_builder{
					Rules: configv1.Int64Rules_builder{Gte: &minTTL}.Build(),
				}.Build(),
			}.Build(),
		},
		ReturnTypes: []*configv1.Field{
			secret,
			configv1.Field_builder{
				Name:          actionReturnResource,
				DisplayName:   "Secret resource",
				Description:   "The created token, so it can be revoked.",
				IsRequired:    true,
				ResourceField: configv1.ResourceField_builder{}.Build(),
			}.Build(),
		},
		ActionType: []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_CREATE},
	}.Build()
}

// ResourceActions registers the issuer on the token type's registry.
func (t *serviceAccountTokenBuilder) ResourceActions(ctx context.Context, registry actions.ActionRegistry) error {
	if err := actions.RegisterWithSecrets(ctx, registry, serviceAccountTokenActionSchema(), t.issueAction); err != nil {
		return fmt.Errorf("baton-grafana: failed to register %s action: %w", ActionIssueServiceAccountToken, err)
	}
	return nil
}

func (t *serviceAccountTokenBuilder) issueAction(
	ctx context.Context,
	args *structpb.Struct,
) (*structpb.Struct, []*v2.PlaintextData, annotations.Annotations, error) {
	return t.issue(ctx, args, time.Now(), rand.Reader)
}

// issue mints one token for the service account the request names. The token
// is named c1-<UTC time>-<random>, so a call never collides with another and
// the issuer needs no duplicate guard.
//
// The resource it returns is built from the token as Grafana reports it after
// the create, so the issued resource and the same token on its next sync are
// one resource: same id, same expiry.
func (t *serviceAccountTokenBuilder) issue(
	ctx context.Context,
	args *structpb.Struct,
	now time.Time,
	random io.Reader,
) (*structpb.Struct, []*v2.PlaintextData, annotations.Annotations, error) {
	subject, err := actions.RequireResourceIDArg(args, actionArgServiceAccountID)
	if err != nil {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument, "baton-grafana: %v", err)
	}
	if subject.GetResourceType() != resourceTypeServiceAccount.Id {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument,
			"baton-grafana: %s must be a %s resource", actionArgServiceAccountID, resourceTypeServiceAccount.Id)
	}
	serviceAccountID, err := strconv.Atoi(subject.GetResource())
	if err != nil || serviceAccountID <= 0 {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument,
			"baton-grafana: service account id %q is not a valid Grafana service account id", subject.GetResource())
	}
	secondsToLive, ok := actions.GetIntArg(args, actionArgSecondsToLive)
	if !ok {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument, "baton-grafana: %s is required", actionArgSecondsToLive)
	}
	if secondsToLive < minSecondsToLive {
		return nil, nil, nil, status.Errorf(codes.InvalidArgument,
			"baton-grafana: %s must be at least %d", actionArgSecondsToLive, minSecondsToLive)
	}

	name, err := newIssuedTokenName(now, random)
	if err != nil {
		return nil, nil, nil, err
	}

	created, _, err := t.client.CreateServiceAccountToken(ctx, serviceAccountID, name, secondsToLive)
	if err != nil {
		// The response can be lost after the provider committed the create. The
		// token, if it exists, is named c1-<time>-<random>, so the next sync
		// inventories it and C1 can revoke it by id.
		return nil, nil, nil, fmt.Errorf("baton-grafana: create service account token: %w", err)
	}
	if created.ID <= 0 || created.Key == "" {
		// No id means no revocation handle and no value means nothing to
		// deliver: both leave a live token nobody can use or revoke.
		return nil, nil, nil, t.failIssuedToken(ctx, serviceAccountID, created.ID, name,
			status.Error(codes.Internal, "baton-grafana: Grafana returned a service account token without an id or value"))
	}

	// The expiry on the resource must be the provider's own, which starts
	// counting when Grafana handles the create. Read it back, uncached.
	token, err := t.readBackIssuedToken(ctx, serviceAccountID, created.ID)
	if err != nil {
		return nil, nil, nil, t.failIssuedToken(ctx, serviceAccountID, created.ID, name,
			fmt.Errorf("baton-grafana: read back the issued service account token: %w", err))
	}
	if token == nil {
		return nil, nil, nil, t.failIssuedToken(ctx, serviceAccountID, created.ID, name,
			status.Errorf(codes.Internal, "baton-grafana: Grafana did not report the service account token it just created (id %d)", created.ID))
	}
	if token.Expiration == nil {
		// The connector never asks for a non-expiring token, so this is the
		// provider contradicting the request.
		return nil, nil, nil, t.failIssuedToken(ctx, serviceAccountID, created.ID, name,
			status.Error(codes.Internal, "baton-grafana: Grafana issued a non-expiring service account token for a request that asked for an expiry"))
	}

	resource, err := serviceAccountTokenResource(subject, token)
	if err != nil {
		return nil, nil, nil, t.failIssuedToken(ctx, serviceAccountID, created.ID, name,
			fmt.Errorf("baton-grafana: build service account token secret resource: %w", err))
	}
	field, err := actions.NewResourceReturnField(actionReturnResource, resource)
	if err != nil {
		return nil, nil, nil, t.failIssuedToken(ctx, serviceAccountID, created.ID, name,
			fmt.Errorf("baton-grafana: build issuer resource return: %w", err))
	}
	// Multipass rejects nulls, so the optional api_key_v2 fields use omitempty.
	plaintext, err := multipass.Plaintext(actionReturnToken, multipass.APIKeyV2{
		KeyValue:   created.Key,
		Provider:   grafanaProviderName,
		BaseURL:    t.client.BaseURL(),
		KeyID:      strconv.FormatInt(created.ID, 10),
		HeaderName: grafanaTokenHeaderName,
		ExpiresAt:  token.Expiration.UTC().Format(time.DateOnly),
	})
	if err != nil {
		return nil, nil, nil, t.failIssuedToken(ctx, serviceAccountID, created.ID, name,
			fmt.Errorf("baton-grafana: encode service account token plaintext: %w", err))
	}

	return actions.NewReturnValues(true, field), []*v2.PlaintextData{plaintext}, nil, nil
}

// readBackIssuedToken returns the token the provider just minted, read from a
// response that bypasses the SDK's HTTP response cache. It is nil when the
// provider does not list it.
func (t *serviceAccountTokenBuilder) readBackIssuedToken(ctx context.Context, serviceAccountID int, tokenID int64) (*grafana.ServiceAccountToken, error) {
	tokens, _, err := t.client.ListServiceAccountTokensFresh(ctx, serviceAccountID)
	if err != nil {
		return nil, err
	}
	for _, token := range tokens {
		if token != nil && token.ID == tokenID {
			return token, nil
		}
	}
	return nil, nil
}

// failIssuedToken removes a token this call minted but cannot return, and
// returns the rejection with the cleanup outcome attached. A cleanup that did
// not complete is part of the returned error: the token is then live at the
// provider and this connector holds its only handle.
func (t *serviceAccountTokenBuilder) failIssuedToken(ctx context.Context, serviceAccountID int, tokenID int64, name string, reason error) error {
	if cleanupErr := t.cleanupIssuedToken(ctx, serviceAccountID, tokenID, name); cleanupErr != nil {
		return errors.Join(reason, fmt.Errorf(
			"baton-grafana: the minted service account token %q could not be removed and may still be live: %w", name, cleanupErr))
	}
	return reason
}

// cleanupIssuedToken removes a token this call minted but cannot hand back. It
// is detached from ctx so it still runs when the caller's context is done, and
// bounded so a provider that stops answering cannot hold the issuance open.
func (t *serviceAccountTokenBuilder) cleanupIssuedToken(ctx context.Context, serviceAccountID int, tokenID int64, name string) error {
	cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), serviceAccountTokenCleanupTimeout)
	defer cancel()

	if tokenID <= 0 {
		// No id came back, so the name the token was created under is the only
		// handle on it.
		tokens, _, err := t.client.ListServiceAccountTokensFresh(cleanupCtx, serviceAccountID)
		if err != nil {
			return fmt.Errorf("look up the service account token to clean up: %w", err)
		}
		existing := findServiceAccountTokenByName(tokens, name)
		if existing == nil {
			return nil
		}
		tokenID = existing.ID
	}

	if _, err := t.client.DeleteServiceAccountToken(cleanupCtx, serviceAccountID, tokenID); err != nil && status.Code(err) != codes.NotFound {
		return fmt.Errorf("delete service account token %d: %w", tokenID, err)
	}
	return nil
}

// newIssuedTokenName names a token the issuer mints: c1-<UTC time>-<random>,
// e.g. c1-20261110T201426Z-k3x9qa.
func newIssuedTokenName(now time.Time, random io.Reader) (string, error) {
	buf := make([]byte, 4)
	if _, err := io.ReadFull(random, buf); err != nil {
		return "", fmt.Errorf("baton-grafana: generate token name: %w", err)
	}
	suffix := strings.ToLower(base32.StdEncoding.WithPadding(base32.NoPadding).EncodeToString(buf))[:issuedTokenNameSuffixLen]
	return "c1-" + now.UTC().Format("20060102T150405Z") + "-" + suffix, nil
}
