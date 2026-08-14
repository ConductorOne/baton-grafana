package connector

import (
	"context"
	"fmt"
	"slices"
	"strconv"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

type serviceAccountBuilder struct {
	client *grafana.Client
}

func newServiceAccountBuilder(client *grafana.Client) *serviceAccountBuilder {
	return &serviceAccountBuilder{client: client}
}

func (s *serviceAccountBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeServiceAccount
}

func serviceAccountResource(sa grafana.ServiceAccount) (*v2.Resource, error) {
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

func (s *serviceAccountBuilder) List(ctx context.Context, _ *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	// GET /api/serviceaccounts/search uses 1-based page; page 1 is the first page.
	bag, page, err := parsePageToken(pToken, &v2.ResourceId{ResourceType: resourceTypeServiceAccount.Id})
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to parse page token: %w", err)
	}
	if page == 0 {
		page = 1
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	}

	serviceAccounts, numNextPage, err := s.client.ListServiceAccounts(ctx, &paginationOpts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list service accounts: %w", err)
	}

	var pageToken string
	if numNextPage > 0 {
		pageToken = strconv.FormatUint(numNextPage, 10)
	}

	next, err := bag.NextToken(pageToken)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to generate next page token: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(serviceAccounts))
	for _, serviceAccount := range serviceAccounts {
		resource, err := serviceAccountResource(serviceAccount)
		if err != nil {
			return nil, "", nil, fmt.Errorf("grafana-connector: failed to create service account resource %q: %w", serviceAccount.Name, err)
		}
		resources = append(resources, resource)
	}

	return resources, next, nil, nil
}

func (s *serviceAccountBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// Grants emits the service account's org role from the search response `role`
// field (Viewer/Editor/Admin). GET /api/org/users does not include service
// accounts, so org-side Grants alone miss that access.
func (s *serviceAccountBuilder) Grants(ctx context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	rawProfile := resource.GetProfile()
	if rawProfile == nil {
		ctxzap.Extract(ctx).Debug(
			"grafana-connector: service account missing profile; skipping org role grant",
			zap.String("resource_id", resource.GetId().GetResource()),
		)
		return nil, "", nil, nil
	}

	role, ok := rs.GetProfileStringValue(rawProfile, profileKeyRole)
	if !ok || !slices.Contains(userRoles, role) {
		ctxzap.Extract(ctx).Debug(
			"grafana-connector: service account has no grantable org role; skipping",
			zap.String("resource_id", resource.GetId().GetResource()),
			zap.String("role", role),
		)
		return nil, "", nil, nil
	}

	orgID, ok := rs.GetProfileStringValue(rawProfile, profileKeyOrgID)
	if !ok || orgID == "" {
		ctxzap.Extract(ctx).Debug(
			"grafana-connector: service account missing org_id; skipping org role grant",
			zap.String("resource_id", resource.GetId().GetResource()),
		)
		return nil, "", nil, nil
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
	)}, "", nil, nil
}
