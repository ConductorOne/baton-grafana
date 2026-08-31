package connector

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

var _ connectorbuilder.ResourceSyncerV2 = (*serviceAccountBuilder)(nil)

type serviceAccountBuilder struct {
	client       *grafana.Client
	resourceType *v2.ResourceType
}

func serviceAccountResourceType(syncOrgs bool) *v2.ResourceType {
	rt := proto.Clone(resourceTypeServiceAccount).(*v2.ResourceType)
	annos := annotations.Annotations(rt.GetAnnotations())
	if syncOrgs {
		annos.Update(&v2.SkipEntitlements{})
	} else {
		annos.Update(&v2.SkipEntitlementsAndGrants{})
	}
	rt.Annotations = annos

	return rt
}

func newServiceAccountBuilder(client *grafana.Client, syncOrgs bool) *serviceAccountBuilder {
	return &serviceAccountBuilder{
		client:       client,
		resourceType: serviceAccountResourceType(syncOrgs),
	}
}

func (s *serviceAccountBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return s.resourceType
}

func serviceAccountResource(sa *grafana.ServiceAccount) (*v2.Resource, error) {
	profile := map[string]any{
		profileKeySAID:       sa.ID,
		profileKeyUID:        sa.UID,
		profileKeyLogin:      sa.Login,
		profileKeyOrgID:      strconv.Itoa(sa.OrgID),
		profileKeyRole:       sa.Role,
		profileKeyTokens:     sa.Tokens,
		profileKeyIsExternal: sa.IsExternal,
		profileKeyIsDisabled: sa.IsDisabled,
	}

	status := v2.Status_RESOURCE_STATUS_ENABLED
	if sa.IsDisabled {
		status = v2.Status_RESOURCE_STATUS_DISABLED
	}

	displayName := sa.Name
	if displayName == "" {
		displayName = sa.Login
	}

	traitOpts := []rs.UserTraitOption{
		rs.WithAccountType(v2.UserTrait_ACCOUNT_TYPE_SERVICE),
	}
	if sa.Login != "" {
		traitOpts = append(traitOpts, rs.WithUserLogin(sa.Login))
	}

	return rs.NewUserResource(
		displayName,
		resourceTypeServiceAccount,
		sa.ID,
		traitOpts,
		rs.WithResourceProfile(profile),
		rs.WithResourceStatus(status, ""),
		rs.WithNHIType(v2.NonHumanIdentityTrait_NHI_TYPE_APP_REGISTRATION, "grafana.service_account"),
	)
}

func (s *serviceAccountBuilder) List(ctx context.Context, _ *v2.ResourceId, attrs rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	bag, page, err := parsePageToken(&attrs.PageToken, &v2.ResourceId{ResourceType: resourceTypeServiceAccount.Id})
	if err != nil {
		return nil, nil, fmt.Errorf("grafana-connector: failed to parse page token: %w", err)
	}

	serviceAccounts, nextPage, annos, err := s.client.ListServiceAccounts(ctx, &grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	})
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to list service accounts: %w", err)
	}

	next, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to generate next page token: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(serviceAccounts))
	for _, serviceAccount := range serviceAccounts {
		if serviceAccount == nil {
			continue
		}
		resource, err := serviceAccountResource(serviceAccount)
		if err != nil {
			return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to create service account resource %q: %w", serviceAccount.Name, err)
		}
		resources = append(resources, resource)
	}

	return resources, &rs.SyncOpResults{NextPageToken: next, Annotations: annos}, nil
}

func (s *serviceAccountBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// Grants emits the SA's org role from the search `role` field. Org-side Grants
// miss SAs because they are absent from GET /api/org/users.
func (s *serviceAccountBuilder) Grants(ctx context.Context, resource *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	rawProfile := resource.GetProfile()
	if rawProfile == nil {
		ctxzap.Extract(ctx).Debug(
			"grafana-connector: service account missing profile; skipping org role grant",
			zap.String("resource_id", resource.GetId().GetResource()),
		)
		return nil, nil, nil
	}

	role, ok := rs.GetProfileStringValue(rawProfile, profileKeyRole)
	if !ok || !slices.Contains(userRoles, role) {
		ctxzap.Extract(ctx).Debug(
			"grafana-connector: service account has no grantable org role; skipping",
			zap.String("resource_id", resource.GetId().GetResource()),
			zap.String("role", role),
		)
		return nil, nil, nil
	}

	orgID, ok := rs.GetProfileStringValue(rawProfile, profileKeyOrgID)
	if !ok || orgID == "" {
		ctxzap.Extract(ctx).Debug(
			"grafana-connector: service account missing org_id; skipping org role grant",
			zap.String("resource_id", resource.GetId().GetResource()),
		)
		return nil, nil, nil
	}

	orgRes := &v2.Resource{
		Id: &v2.ResourceId{
			ResourceType: resourceTypeOrg.Id,
			Resource:     orgID,
		},
	}

	// Sync-only: org Grant/Revoke accept users only. Mark immutable so C1 does
	// not offer revoke of an SA org role that the connector cannot change.
	return []*v2.Grant{grant.NewGrant(
		orgRes,
		role,
		resource.Id,
		grant.WithAnnotation(&v2.GrantImmutable{}),
	)}, nil, nil
}
