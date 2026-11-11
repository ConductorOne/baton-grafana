package connector

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/json"
	"io"
	"net/http"
	"testing"
	"time"

	filippoage "filippo.io/age"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/types/known/structpb"

	configv1 "github.com/conductorone/baton-sdk/pb/c1/config/v1"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/actions"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"

	"github.com/conductorone/baton-grafana/local_vendor/baton-multipass-sdk/pkg/multipass"
	"github.com/conductorone/baton-grafana/pkg/grafana"
)

// issuerExpiry is the expiry the fake provider reports for a freshly minted token.
var issuerExpiry = time.Date(2026, 12, 1, 8, 30, 0, 0, time.UTC)

const issuerSecret = "glsa_one_time_value" //nolint:gosec // Fake provider value used only by tests.

func newIssuerFixture() *tokenFixture {
	exp := issuerExpiry
	return &tokenFixture{createdExpiration: &exp}
}

// registeredIssuerSchemas runs the connector's syncers' ResourceActions into one
// ActionManager and returns the schemas registered for a resource type.
func registeredIssuerSchemas(t *testing.T, g *Grafana, resourceTypeID string) []*v2.BatonActionSchema {
	t.Helper()
	mgr := newIssuerActionManager(t, g)
	schemas, _, err := mgr.ListActionSchemas(context.Background(), resourceTypeID)
	require.NoError(t, err)
	return schemas
}

func newIssuerActionManager(t *testing.T, g *Grafana) *actions.ActionManager {
	t.Helper()
	ctx := context.Background()
	mgr := actions.NewActionManager(ctx)
	for _, syncer := range g.ResourceSyncers(ctx) {
		provider, ok := syncer.(connectorbuilder.ResourceActionProvider)
		if !ok {
			continue
		}
		registry, err := mgr.GetTypeRegistry(ctx, syncer.ResourceType(ctx).GetId())
		require.NoError(t, err)
		require.NoError(t, provider.ResourceActions(ctx, registry))
	}
	return mgr
}

func issuerGrafana(t *testing.T, fixture *tokenFixture) *Grafana {
	t.Helper()
	return &Grafana{
		client:                   newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)),
		SyncServiceAccountTokens: true,
	}
}

func issuerTokenBuilder(t *testing.T, fixture *tokenFixture) *serviceAccountTokenBuilder {
	t.Helper()
	return newServiceAccountTokenBuilder(newUncachedCloudClientForTest(t, newTokenFixtureServer(t, fixture)))
}

func issuerArgs(t *testing.T, resourceType, resource string, secondsToLive any) *structpb.Struct {
	t.Helper()
	fields := map[string]any{}
	if resourceType != "" {
		fields[actionArgServiceAccountID] = map[string]any{"resource_type_id": resourceType, "resource_id": resource}
	}
	if secondsToLive != nil {
		fields[actionArgSecondsToLive] = secondsToLive
	}
	args, err := structpb.NewStruct(fields)
	require.NoError(t, err)
	return args
}

func validIssuerArgs(t *testing.T) *structpb.Struct {
	return issuerArgs(t, resourceTypeServiceAccount.Id, "7", 3600)
}

// requireResourceReturn checks the returned resource's id and that its
// SecretTrait survived the conversion.
func requireResourceReturn(t *testing.T, response *structpb.Struct, resourceTypeID, resourceID string) {
	t.Helper()
	res := response.GetFields()[actionReturnResource].GetStructValue()
	require.NotNil(t, res, "the response carries the created secret resource")
	id := res.GetFields()["resourceId"].GetStructValue().GetFields()
	require.Equal(t, resourceTypeID, id["resourceTypeId"].GetStringValue())
	require.Equal(t, resourceID, id["resourceId"].GetStringValue())
	require.Contains(t, res.GetFields()["annotations"].String(), "SecretTrait")
}

// 1. Schema shape.
func TestIssuerSchemaShape(t *testing.T) {
	schemas := registeredIssuerSchemas(t, issuerGrafana(t, newIssuerFixture()), resourceTypeServiceAccountToken.Id)
	require.Len(t, schemas, 1, "one RESOURCE_CREATE per secret type")
	schema := schemas[0]
	require.Equal(t, ActionIssueServiceAccountToken, schema.GetName())
	require.Equal(t, resourceTypeServiceAccountToken.Id, schema.GetResourceTypeId())
	require.Equal(t, []v2.ActionType{v2.ActionType_ACTION_TYPE_RESOURCE_CREATE}, schema.GetActionType())

	var secrets, resources []*configv1.Field
	for _, f := range schema.GetReturnTypes() {
		if f.GetIsSecret() {
			secrets = append(secrets, f)
		}
		if f.HasResourceField() {
			resources = append(resources, f)
		}
	}
	require.Len(t, secrets, 1, "exactly one secret return")
	require.Equal(t, multipass.ContentType, secrets[0].GetContentType())
	require.Len(t, resources, 1, "exactly one resource return")
	require.Equal(t, actionReturnResource, resources[0].GetName())

	var subjects []*configv1.Field
	args := map[string]*configv1.Field{}
	for _, a := range schema.GetArguments() {
		args[a.GetName()] = a
		if a.HasResourceIdField() {
			subjects = append(subjects, a)
		}
	}
	require.Len(t, subjects, 1, "at most one ResourceIdField")
	require.Equal(t, actionArgServiceAccountID, subjects[0].GetName())
	require.True(t, subjects[0].GetIsRequired())
	require.Equal(t, []string{resourceTypeServiceAccount.Id}, subjects[0].GetResourceIdField().GetRules().GetAllowedResourceTypeIds())

	// The lifetime is the one admin-fixed argument.
	require.Len(t, schema.GetArguments(), 2)
	ttl := args[actionArgSecondsToLive]
	require.NotNil(t, ttl)
	require.True(t, ttl.GetIsRequired())
	require.True(t, ttl.HasIntField())
	require.Equal(t, minSecondsToLive, ttl.GetIntField().GetRules().GetGte())
	require.NoError(t, schema.GetArguments()[0].Validate())
	require.NoError(t, ttl.Validate())
}

// 2. Handler result.
func TestIssuerResultKeepsTheSecretTraitAndTheCompositeID(t *testing.T) {
	fixture := newIssuerFixture()
	builder := issuerTokenBuilder(t, fixture)
	now := time.Date(2026, 11, 10, 20, 14, 26, 0, time.UTC)

	response, plaintext, _, err := builder.issue(context.Background(), validIssuerArgs(t), now, bytes.NewReader([]byte{0, 0, 0, 0}))
	require.NoError(t, err)

	// The id is the one sync reports and delete accepts: <service account id>.<token id>.
	requireResourceReturn(t, response, resourceTypeServiceAccountToken.Id, "7.41")
	require.Contains(t, response.GetFields()[actionReturnResource].GetStructValue().String(), "2026-12-01",
		"the provider's own expiry is on the SecretTrait")

	require.Len(t, plaintext, 1)
	require.Equal(t, actionReturnToken, plaintext[0].GetName())
	require.Equal(t, multipass.ContentType, plaintext[0].GetSchema(), "the SDK does not copy the schema onto the plaintext")
	var doc map[string]any
	require.NoError(t, json.Unmarshal(plaintext[0].GetBytes(), &doc))
	require.Equal(t, map[string]any{
		"key_value":   issuerSecret,
		"provider":    "grafana",
		"base_url":    builder.client.BaseURL(),
		"key_id":      "41",
		"header_name": "Authorization",
		"expires_at":  "2026-12-01",
	}, doc, "no nulls, no unknown keys, no scopes")

	var create map[string]any
	for _, req := range fixture.recorded() {
		if req.method == http.MethodPost {
			require.Equal(t, "/api/serviceaccounts/7/tokens", req.path)
			create = req.body
		}
	}
	require.Equal(t, "c1-20261110T201426Z-aaaaaa", create["name"], "named by timestamp and random suffix")
	require.EqualValues(t, 3600, create["secondsToLive"], "the admin-fixed lifetime is sent as is, never zero")
}

// 3. Bad arguments.
func TestIssuerRejectsBadArguments(t *testing.T) {
	fixture := newIssuerFixture()
	builder := issuerTokenBuilder(t, fixture)

	for name, args := range map[string]*structpb.Struct{
		"missing subject":          issuerArgs(t, "", "", 3600),
		"wrong subject type":       issuerArgs(t, resourceTypeUser.Id, "7", 3600),
		"empty subject id":         issuerArgs(t, resourceTypeServiceAccount.Id, "", 3600),
		"non-numeric subject id":   issuerArgs(t, resourceTypeServiceAccount.Id, "seven", 3600),
		"zero subject id":          issuerArgs(t, resourceTypeServiceAccount.Id, "0", 3600),
		"missing lifetime":         issuerArgs(t, resourceTypeServiceAccount.Id, "7", nil),
		"zero lifetime":            issuerArgs(t, resourceTypeServiceAccount.Id, "7", 0),
		"negative lifetime":        issuerArgs(t, resourceTypeServiceAccount.Id, "7", -5),
		"lifetime below the floor": issuerArgs(t, resourceTypeServiceAccount.Id, "7", minSecondsToLive-1),
	} {
		t.Run(name, func(t *testing.T) {
			_, _, _, err := builder.issue(context.Background(), args, time.Now(), rand.Reader)
			require.Equal(t, codes.InvalidArgument, status.Code(err), "got %v", err)
		})
	}
	require.Empty(t, fixture.recorded(), "a rejected request never reaches the provider")
}

// 4. ActionManager round trip.
func TestIssuerThroughActionManagerEncryptsTheSecret(t *testing.T) {
	ctx := context.Background()
	mgr := newIssuerActionManager(t, issuerGrafana(t, newIssuerFixture()))

	identity, err := filippoage.GenerateHybridIdentity()
	require.NoError(t, err)
	recipient := v2.EncryptionConfig_builder{
		AgeRecipientConfig: v2.EncryptionConfig_AgeRecipientConfig_builder{Recipient: identity.Recipient().String()}.Build(),
	}.Build()

	_, actionStatus, response, encrypted, _, err := mgr.InvokeActionWithWaitAndEncryption(
		ctx, ActionIssueServiceAccountToken, resourceTypeServiceAccountToken.Id,
		validIssuerArgs(t), 5*time.Second, []*v2.EncryptionConfig{recipient})
	require.NoError(t, err)
	require.Equal(t, v2.BatonActionStatus_BATON_ACTION_STATUS_COMPLETE, actionStatus)

	require.Len(t, encrypted, 1)
	require.Equal(t, actionReturnToken, encrypted[0].GetName())
	require.Equal(t, multipass.ContentType, encrypted[0].GetSchema())
	require.NotContains(t, string(encrypted[0].GetEncryptedBytes()), issuerSecret)
	require.NotContains(t, response.String(), issuerSecret, "the public response never carries the token")
	requireResourceReturn(t, response, resourceTypeServiceAccountToken.Id, "7.41")

	reader, err := filippoage.Decrypt(bytes.NewReader(encrypted[0].GetEncryptedBytes()), identity)
	require.NoError(t, err)
	decrypted, err := io.ReadAll(reader)
	require.NoError(t, err)
	require.Contains(t, string(decrypted), `"key_value":"`+issuerSecret+`"`)
}

// The SDK refuses a secret result with no encryption config.
func TestIssuerRefusesToRunWithoutAnEncryptionConfig(t *testing.T) {
	fixture := newIssuerFixture()
	mgr := newIssuerActionManager(t, issuerGrafana(t, fixture))

	_, actionStatus, response, _, _, _ := mgr.InvokeActionWithWaitAndEncryption(
		context.Background(), ActionIssueServiceAccountToken, resourceTypeServiceAccountToken.Id,
		validIssuerArgs(t), 5*time.Second, nil)
	require.NotEqual(t, v2.BatonActionStatus_BATON_ACTION_STATUS_COMPLETE, actionStatus)
	require.NotContains(t, response.String(), issuerSecret)
}

// 5. Create then delete.
func TestIssuedResourceIDIsWhatDeleteAccepts(t *testing.T) {
	fixture := newIssuerFixture()
	builder := issuerTokenBuilder(t, fixture)

	response, _, _, err := builder.issue(context.Background(), validIssuerArgs(t), time.Now(), rand.Reader)
	require.NoError(t, err)

	id := response.GetFields()[actionReturnResource].GetStructValue().
		GetFields()["resourceId"].GetStructValue().GetFields()
	_, err = builder.Delete(context.Background(), &v2.ResourceId{
		ResourceType: id["resourceTypeId"].GetStringValue(),
		Resource:     id["resourceId"].GetStringValue(),
	}, nil)
	require.NoError(t, err)

	require.Equal(t, 1, fixture.countMethodPath(http.MethodDelete, "/api/serviceaccounts/7/tokens/41"),
		"the provider receives a delete for the token the issuer created")
}

// The issued resource and the same token on its next sync are one resource.
func TestIssuedResourceMatchesTheSyncedResource(t *testing.T) {
	fixture := newIssuerFixture()
	builder := issuerTokenBuilder(t, fixture)
	_, _, _, err := builder.issue(context.Background(), validIssuerArgs(t), time.Now(), rand.Reader)
	require.NoError(t, err)

	synced, err := serviceAccountTokenResource(
		&v2.ResourceId{ResourceType: resourceTypeServiceAccount.Id, Resource: "7"},
		&grafana.ServiceAccountToken{ID: 41, Name: "x", Expiration: &issuerExpiry})
	require.NoError(t, err)
	require.Equal(t, "7.41", synced.GetId().GetResource())
	require.Equal(t, resourceTypeServiceAccountToken.Id, synced.GetId().GetResourceType())
}

func TestIssuerCleansUpATokenItCannotReturn(t *testing.T) {
	t.Run("non-expiring token from the provider", func(t *testing.T) {
		fixture := &tokenFixture{} // createdExpiration nil: the provider reports no expiry
		builder := issuerTokenBuilder(t, fixture)
		_, _, _, err := builder.issue(context.Background(), validIssuerArgs(t), time.Now(), rand.Reader)
		require.Equal(t, codes.Internal, status.Code(err))
		require.Equal(t, 1, fixture.countMethodPath(http.MethodDelete, "/api/serviceaccounts/7/tokens/41"))
	})

	t.Run("no id or value in the create response", func(t *testing.T) {
		fixture := newIssuerFixture()
		fixture.createResponse = &grafana.CreatedServiceAccountToken{}
		fixture.tokensAfterCreate = map[int][]*grafana.ServiceAccountToken{7: {}}
		builder := issuerTokenBuilder(t, fixture)
		_, _, _, err := builder.issue(context.Background(), validIssuerArgs(t), time.Now(), rand.Reader)
		require.Equal(t, codes.Internal, status.Code(err))
	})

	t.Run("token missing from the read back", func(t *testing.T) {
		fixture := newIssuerFixture()
		fixture.tokensAfterCreate = map[int][]*grafana.ServiceAccountToken{7: {}}
		builder := issuerTokenBuilder(t, fixture)
		_, _, _, err := builder.issue(context.Background(), validIssuerArgs(t), time.Now(), rand.Reader)
		require.Equal(t, codes.Internal, status.Code(err))
		require.Equal(t, 1, fixture.countMethodPath(http.MethodDelete, "/api/serviceaccounts/7/tokens/41"))
	})

	t.Run("a failed cleanup is part of the error", func(t *testing.T) {
		fixture := &tokenFixture{deleteStatus: http.StatusInternalServerError}
		builder := issuerTokenBuilder(t, fixture)
		_, _, _, err := builder.issue(context.Background(), validIssuerArgs(t), time.Now(), rand.Reader)
		require.ErrorContains(t, err, "may still be live")
	})
}

func TestNewIssuedTokenName(t *testing.T) {
	now := time.Date(2026, 1, 2, 15, 4, 5, 0, time.FixedZone("x", 3600))
	name, err := newIssuedTokenName(now, bytes.NewReader([]byte{0, 0, 0, 0}))
	require.NoError(t, err)
	require.Equal(t, "c1-20260102T140405Z-aaaaaa", name, "UTC timestamp, base32 suffix")

	again, err := newIssuedTokenName(now, rand.Reader)
	require.NoError(t, err)
	require.Regexp(t, `^c1-\d{8}T\d{6}Z-[a-z2-7]{6}$`, again)

	_, err = newIssuedTokenName(now, bytes.NewReader(nil))
	require.Error(t, err)
}
