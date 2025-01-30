package connector

import (
	"context"
	"fmt"
	"strings"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	ent "github.com/conductorone/baton-sdk/pkg/types/entitlement"
	"github.com/conductorone/baton-sdk/pkg/types/grant"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"golang.org/x/exp/slices"
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
func orgResource(ctx context.Context, org *grafana.Organization) (*v2.Resource, error) {
	resource, err := rs.NewResource(
		titleCase(org.Name),
		resourceTypeOrg,
		org.ID,
		rs.WithAnnotation(
			&v2.ChildResourceType{ResourceTypeId: resourceTypeUser.Id},
		),
	)

	if err != nil {
		return nil, err
	}

	return resource, nil
}

// List returns all the organizations from the database as resource objects.
func (o *orgResourceType) List(ctx context.Context, _ *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	bag, page, err := parsePageToken(pToken.Token, &v2.ResourceId{ResourceType: resourceTypeOrg.Id})
	if err != nil {
		return nil, "", nil, err
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: uint(page),
	}

	orgs, nextPage, err := o.client.ListOrganizations(ctx, &paginationOpts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list organizations: %w", err)
	}

	next, err := bag.NextToken(fmt.Sprintf("%d", nextPage))
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Resource
	for _, org := range orgs {
		// check for valid orgs and skip if not
		if len(o.orgs) != 0 {
			if _, ok := o.orgs[org.Name]; !ok {
				continue
			}
		}

		orgCopy := org

		resource, err := orgResource(ctx, &orgCopy)
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, resource)
	}

	return rv, next, nil, nil
}

// Entitlements returns a slice of entitlements for possible user roles under organization (Viewer, Editor, Admin).
func (o *orgResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	var rv []*v2.Entitlement

	for _, role := range userRoles {
		roleOptions := []ent.EntitlementOption{
			ent.WithGrantableTo(resourceTypeUser),
			ent.WithDisplayName(fmt.Sprintf("%s %s", resource.DisplayName, role)),
			ent.WithDescription(fmt.Sprintf("%s role in %s grafana organization", titleCase(role), resource.DisplayName)),
		}

		rv = append(rv, ent.NewPermissionEntitlement(resource, role, roleOptions...))
	}

	return rv, "", nil, nil
}

// Grants returns a slice of grants for each user and their set role under organization.
func (o *orgResourceType) Grants(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	bag, page, err := parsePageToken(pToken.Token, resource.Id)
	if err != nil {
		return nil, "", nil, err
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: uint(page),
	}

	users, nextPage, err := o.client.ListUsers(ctx, resource.Id.Resource, &paginationOpts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list users under organization %s: %w", resource.Id.Resource, err)
	}

	next, err := bag.NextToken(fmt.Sprintf("%d", nextPage))
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Grant
	for _, user := range users {
		role := strings.ToLower(user.Role)

		// check for valid roles and skip if not
		if !slices.Contains(userRoles, role) {
			continue
		}

		userCopy := user
		ur, err := userResource(ctx, &userCopy, resource.Id)
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, grant.NewGrant(resource, role, ur.Id))
	}

	return rv, next, nil, nil
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
