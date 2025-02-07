package connector

import (
	"context"
	"fmt"
	"strconv"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
)

type userBuilder struct {
	resourceType *v2.ResourceType
	client       *grafana.Client
}

// ResourceType returns the Baton resource type for users.
func (u *userBuilder) ResourceType(ctx context.Context) *v2.ResourceType {
	return resourceTypeUser
}

// userResource creates a Baton resource for a Grafana user.
func userResource(user *grafana.User) (*v2.Resource, error) {
	profile := map[string]interface{}{
		"full_name": user.Name,
		"login":     user.Login,
		"user_id":   user.ID,
		"email":     user.Email,
	}

	status := v2.UserTrait_Status_STATUS_ENABLED
	if user.IsDisabled {
		status = v2.UserTrait_Status_STATUS_DISABLED
	}

	userTraitOptions := []rs.UserTraitOption{
		rs.WithUserProfile(profile),
		rs.WithStatus(status),
		rs.WithEmail(user.Email, true),
	}

	resource, err := rs.NewUserResource(
		user.Login,
		resourceTypeUser,
		user.ID,
		userTraitOptions,
	)

	if err != nil {
		return nil, err
	}

	return resource, nil
}

// List fetches all users in Grafana.
func (u *userBuilder) List(ctx context.Context, _ *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	// Parse pagination token. If Token is an empty string, the function returns 0.
	bag, page, err := parsePageToken(pToken, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to parse page token: %w", err)
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	}

	// Fetch users from Grafana
	users, numNextPage, err := u.client.ListUsers(ctx, &paginationOpts)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: failed to list users: %w", err)
	}

	// Generate next page token
	var pageToken string
	if numNextPage > 0 {
		pageToken = strconv.FormatUint(numNextPage, 10)
	}

	next, err := bag.NextToken(pageToken)
	if err != nil {
		return nil, "", nil, fmt.Errorf("failed to generate next token: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(users))

	// Convert users to resources
	for _, user := range users {
		ur, err := userResource(&user)
		if err != nil {
			return nil, "", nil, fmt.Errorf("failed to create resource for user %s: %w", user.Email, err)
		}
		resources = append(resources, ur)
	}

	return resources, next, nil, nil
}

// Entitlements returns an empty list for users.
func (u *userBuilder) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// Grants returns an empty list for users.
func (u *userBuilder) Grants(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// newUserBuilder initializes a user resource type.
func newUserBuilder(client *grafana.Client) *userBuilder {
	return &userBuilder{
		resourceType: resourceTypeUser,
		client:       client,
	}
}
