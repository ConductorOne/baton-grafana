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
	ent "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
)

const (
	roleViewer = "Viewer"
	roleEditor = "Editor"
	roleAdmin  = "Admin"
)

var userRoles = []string{roleViewer, roleEditor, roleAdmin}

type orgResourceType struct {
	resourceType *v2.ResourceType
	client       *grafana.Client
	orgs         map[string]*struct{}
}

func (o *orgResourceType) ResourceType(ctx context.Context) *v2.ResourceType {
	return resourceTypeOrg
}

// Create a new connector resource for an grafana organization.
func orgResource(ctx context.Context, org grafana.Organization) (*v2.Resource, error) {
	resource, err := rs.NewResource(
		titleCase(org.Name),
		resourceTypeOrg,
		org.ID,
		// rs.WithAnnotation(
		// 	&v2.ChildResourceType{ResourceTypeId: resourceTypeUser.Id},
		// ),
	)

	if err != nil {
		return nil, err
	}

	return resource, nil
}

// List returns all the organizations from the database as resource objects.
func (o *orgResourceType) List(ctx context.Context, parentId *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	// l := ctxzap.Extract(ctx)
	// l.Info(fmt.Sprintf("(1) LISTING ORGS: parentID: %s -||- size: %d -||- token: %s <|||||", parentId, pToken.Size, pToken.Token))

	// Parse pagination token
	bag, page, err := parsePageToken(pToken.Token, &v2.ResourceId{ResourceType: resourceTypeOrg.Id})
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to parse page token: %w", err)
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	}
	// l.Info(fmt.Sprintf("(2) LISTING ORGS: parentID: %s -||- size: %d -||- token: %s <|||||", parentId, pToken.Size, pToken.Token))
	// Fetch organizations from Grafana
	orgs, numNextPage, err := o.client.ListOrganizations(ctx, &paginationOpts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list organizations: %w", err)
	}
	// l.Info(fmt.Sprintf("(3) LISTING ORGS: %v <|||||", orgs))

	// Determine next page token
	var pageToken string
	if numNextPage > 0 {
		pageToken = strconv.FormatUint(numNextPage, 10)
	}
	// l.Info(fmt.Sprintf("(4) LISTING ORGS: NextpageToken: %s -||- numNextPage: %d -||- pToken.Token: %s <|||||", pageToken, numNextPage, pToken.Token))
	next, err := bag.NextToken(pageToken)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to generate next page token: %w", err)
	}

	// Iterate over organizations and filter valid ones
	resources := make([]*v2.Resource, 0, len(orgs))
	for _, org := range orgs {
		// Convert organization to a v2.Resource
		resource, err := orgResource(ctx, org)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to create resource for org %s: %w", org.Name, err)
		}

		resources = append(resources, resource)
	}

	return resources, next, nil, nil
}

// Entitlements returns a slice of entitlements for possible user roles under organization (Viewer, Editor, Admin).
func (o *orgResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	// Preallocate slice for efficiency
	entitlements := make([]*v2.Entitlement, 0, len(userRoles))

	for _, role := range userRoles {
		// Generate display name and description
		displayName := fmt.Sprintf("%s %s", resource.DisplayName, role)
		description := fmt.Sprintf("%s role in %s Grafana organization", titleCase(role), resource.DisplayName)

		// Define entitlement options
		entitlementOptions := []ent.EntitlementOption{
			ent.WithGrantableTo(resourceTypeUser),
			ent.WithDisplayName(displayName),
			ent.WithDescription(description),
		}

		// Append new entitlement to the slice
		entitlements = append(entitlements, ent.NewPermissionEntitlement(resource, role, entitlementOptions...))
	}

	return entitlements, "", nil, nil
}

// Grants returns a slice of grants for each user and their set role under organization.
func (o *orgResourceType) Grants(ctx context.Context, parentResource *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	// Fetch users under the organization (The endpoint used in this method does not support pagination.)
	usersByOrgResponse, err := o.client.ListUsersByOrg(ctx, parentResource.Id.Resource)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list users under organization %s: %w", parentResource.Id.Resource, err)
	}

	grants := make([]*v2.Grant, 0, len(usersByOrgResponse))
	// grants := make([]*v2.Grant, 0, 5)

	// Iterate through users and create grants
	for _, userByOrg := range usersByOrgResponse {
		// Skip users with invalid roles
		if !slices.Contains(userRoles, userByOrg.Role) {
			continue
		}

		// Convert UserByOrg to User only when needed
		user := userByOrg.ToUser()
		ur, err := userResource(ctx, &user)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to generate user resource for %s: %w", user.Email, err)
		}

		// Append grant to the slice
		grants = append(grants, grant.NewGrant(parentResource, userByOrg.Role, ur.Id))
	}

	return grants, "", nil, nil
}

func orgBuilder(client *grafana.Client, orgs []string) *orgResourceType {
	orgMap := make(map[string]*struct{})
	for _, org := range orgs {
		orgMap[org] = &struct{}{}
	}

	return &orgResourceType{
		resourceType: resourceTypeOrg,
		client:       client,
		orgs:         orgMap,
	}
}
