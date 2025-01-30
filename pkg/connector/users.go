package connector

import (
	"context"
	"fmt"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
)

// userResourceType represents the user entity in Grafana.
type userResourceType struct {
	resourceType *v2.ResourceType
	client       *grafana.Client
}

// ResourceType returns the Baton resource type for users.
func (u *userResourceType) ResourceType(ctx context.Context) *v2.ResourceType {
	return resourceTypeUser
}

// userResource creates a Baton resource for a Grafana user.
func userResource(ctx context.Context, user *grafana.User, parentId *v2.ResourceId) (*v2.Resource, error) {
	profile := map[string]interface{}{
		"login":    user.Login,
		"user_id":  user.ID,
		"email":    user.Email,
		"is_admin": user.Role == roleAdmin,
	}

	userTraitOptions := []rs.UserTraitOption{
		rs.WithUserProfile(profile),
		rs.WithStatus(v2.UserTrait_Status_STATUS_ENABLED),
		rs.WithEmail(user.Email, true),
	}

	resource, err := rs.NewUserResource(
		user.Login,
		resourceTypeUser,
		user.ID,
		userTraitOptions,
		rs.WithParentResourceID(parentId),
	)

	if err != nil {
		return nil, err
	}

	return resource, nil
}

func (u *userResourceType) List(ctx context.Context, parentId *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)
	l.Info(fmt.Sprintf("LISTING USERS: parentID: %s --- size: %d token: --- %s", parentId, pToken.Size, pToken.Token))

	if parentId == nil {
		return nil, "", nil, nil
	}

	bag, page, err := parsePageToken(pToken.Token, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
	if err != nil {
		return nil, "", nil, err
	}

	// l.Info(fmt.Sprintf("LISTING USERS: Page: %d  --- bag: %v", page, bag))

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: uint(page),
	}

	users, nextPage, err := u.client.ListUsers(ctx, parentId.Resource, &paginationOpts)
	if err != nil {
		return nil, "", nil, err
	}

	next, err := bag.NextToken(fmt.Sprintf("%d", nextPage))
	if err != nil {
		return nil, "", nil, err
	}

	var rv []*v2.Resource
	for _, user := range users {
		userCopy := user

		ur, err := userResource(ctx, &userCopy, parentId)
		if err != nil {
			return nil, "", nil, err
		}

		rv = append(rv, ur)
	}

	return rv, next, nil, nil
}

// Entitlements returns an empty list for users.
func (u *userResourceType) Entitlements(_ context.Context, resource *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// Grants returns an empty list for users.
func (u *userResourceType) Grants(ctx context.Context, resource *v2.Resource, pToken *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// userBuilder initializes a user resource type.
func userBuilder(client *grafana.Client) *userResourceType {
	return &userResourceType{
		resourceType: resourceTypeUser,
		client:       client,
	}
}
