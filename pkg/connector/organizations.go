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

type orgBuilder struct {
	resourceType *v2.ResourceType
	client       *grafana.Client
}

func (o *orgBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return resourceTypeOrg
}

// Create a new connector resource for an grafana organization.
func orgResource(org grafana.Organization) (*v2.Resource, error) {
	resource, err := rs.NewResource(
		titleCase(org.Name),
		resourceTypeOrg,
		org.ID,
	)

	if err != nil {
		return nil, err
	}

	return resource, nil
}

// List returns all the organizations.
func (o *orgBuilder) List(ctx context.Context, _ *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	// Parse pagination token. If Token is an empty string, the function returns 0.
	bag, page, err := parsePageToken(pToken, &v2.ResourceId{ResourceType: resourceTypeOrg.Id})
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to parse page token: %w", err)
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	}

	// Fetch organizations from Grafana
	orgs, numNextPage, err := o.client.ListOrganizations(ctx, &paginationOpts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list organizations: %w", err)
	}

	// Determine next page token
	var pageToken string
	if numNextPage > 0 {
		pageToken = strconv.FormatUint(numNextPage, 10)
	}

	next, err := bag.NextToken(pageToken)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to generate next page token: %w", err)
	}

	// Iterate over organizations and filter valid ones
	resources := make([]*v2.Resource, 0, len(orgs))
	for _, org := range orgs {
		// Convert organization to a v2.Resource
		resource, err := orgResource(org)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to create resource for org %s: %w", org.Name, err)
		}

		resources = append(resources, resource)
	}

	return resources, next, nil, nil
}

// Entitlements returns a slice of entitlements for possible user roles under organization (Viewer, Editor, Admin).
func (o *orgBuilder) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
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
func (o *orgBuilder) Grants(ctx context.Context, parentResource *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	// Fetch users under the organization (The endpoint used in this method does not support pagination.)
	usersByOrgResponse, err := o.client.ListUsersByOrg(ctx, parentResource.Id.Resource)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list users under organization %s: %w", parentResource.Id.Resource, err)
	}

	grants := make([]*v2.Grant, 0, len(usersByOrgResponse))

	// Iterate through users and create grants
	for _, userByOrg := range usersByOrgResponse {
		// Skip users with invalid roles
		if !slices.Contains(userRoles, userByOrg.Role) {
			continue
		}

		// Convert UserByOrg to User only when needed
		user := userByOrg.ToUser()
		ur, err := userResource(&user)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to generate user resource for %s: %w", user.Email, err)
		}

		// Append grant to the slice
		grants = append(grants, grant.NewGrant(parentResource, userByOrg.Role, ur.Id))
	}

	return grants, "", nil, nil
}

func newOrgBuilder(client *grafana.Client) *orgBuilder {
	return &orgBuilder{
		resourceType: resourceTypeOrg,
		client:       client,
	}
}
