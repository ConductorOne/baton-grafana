package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/crypto"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	rs "github.com/conductorone/baton-sdk/pkg/types/resource"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

type userBuilder struct {
	resourceType *v2.ResourceType
	client       *grafana.Client
}

var _ connectorbuilder.AccountManager = &userBuilder{}

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

// CreateAccountCapabilityDetails indicates the credential options this connector supports.
func (u *userBuilder) CreateAccountCapabilityDetails(ctx context.Context) (*v2.CredentialDetailsAccountProvisioning, annotations.Annotations, error) {
	return &v2.CredentialDetailsAccountProvisioning{
		SupportedCredentialOptions: []v2.CapabilityDetailCredentialOption{
			v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_RANDOM_PASSWORD,
			v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_ENCRYPTED_PASSWORD,
		},
		PreferredCredentialOption: v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_RANDOM_PASSWORD,
	}, nil, nil
}

// CreateAccount provisions a new user in Grafana.
func (u *userBuilder) CreateAccount(
	ctx context.Context,
	accountInfo *v2.AccountInfo,
	credentialOptions *v2.LocalCredentialOptions,
) (connectorbuilder.CreateAccountResponse, []*v2.PlaintextData, annotations.Annotations, error) {
	// Extract required information from account profile
	l := ctxzap.Extract(ctx)

	// Extract user information from profile
	emailVal := accountInfo.Profile.GetFields()["email"]
	if emailVal == nil || emailVal.GetStringValue() == "" {
		return nil, nil, nil, fmt.Errorf("grafana-connector: email is required for account creation")
	}
	email := emailVal.GetStringValue()

	// Get full name from profile or use email as fallback
	name := email
	nameVal := accountInfo.Profile.GetFields()["full_name"]
	if nameVal != nil && nameVal.GetStringValue() != "" {
		name = nameVal.GetStringValue()
	}

	// Use email as login if not provided
	login := email
	loginVal := accountInfo.Profile.GetFields()["login"]
	if loginVal != nil && loginVal.GetStringValue() != "" {
		login = loginVal.GetStringValue()
	}

	// Check if an organization ID was provided
	var orgId int
	orgIdVal := accountInfo.Profile.GetFields()["org_id"]
	if orgIdVal != nil && orgIdVal.GetStringValue() != "" {
		// Convert org_id string to int
		var convErr error
		orgId, convErr = strconv.Atoi(orgIdVal.GetStringValue())
		if convErr != nil {
			l.Error("Invalid organization ID format", zap.Error(convErr), zap.String("org_id", orgIdVal.GetStringValue()))
			return nil, nil, nil, fmt.Errorf("grafana-connector: invalid organization ID: %w", convErr)
		}
	}

	password, err := crypto.GeneratePassword(ctx, credentialOptions)
	if err != nil {
		l.Error("Failed to generate random password", zap.Error(err))
		return nil, nil, nil, fmt.Errorf("grafana-connector: failed to generate random password: %w", err)
	}

	// Prepare request to create user
	createUserReq := &grafana.CreateUserRequest{
		Name:     name,
		Email:    email,
		Login:    login,
		Password: password,
	}

	// Add organization ID if provided
	if orgId > 0 {
		createUserReq.OrgId = orgId
	}

	// Create the user in Grafana
	user, err := u.client.CreateUser(ctx, createUserReq)
	if err != nil {
		// Check if the error indicates the user already exists
		if errors.Is(err, grafana.ErrUserAlreadyExists) {
			l.Warn("User already exists in Grafana", zap.String("email", email), zap.String("login", login))

			// Try to find the user directly by login or email
			existingUser, findErr := u.client.GetUserByLoginOrEmail(ctx, login)
			if findErr != nil {
				// Try with email if login failed
				if login != email {
					existingUser, findErr = u.client.GetUserByLoginOrEmail(ctx, email)
				}

				if findErr != nil {
					l.Error("Could not find existing user after 412 error",
						zap.Error(findErr),
						zap.String("email", email),
						zap.String("login", login))

					// Return the original error if we can't find the user
					return nil, nil, nil, fmt.Errorf("grafana-connector: failed to create user and couldn't find existing user: %w", err)
				}
			}

			// We found the user, create a resource for them
			resource, resourceErr := userResource(existingUser)
			if resourceErr != nil {
				l.Error("Failed to create resource for existing user", zap.Error(resourceErr))
				return nil, nil, nil, fmt.Errorf("grafana-connector: failed to create resource for existing user: %w", resourceErr)
			}

			successResult := &v2.CreateAccountResponse_SuccessResult{
				Resource: resource,
			}

			// Return with GrantAlreadyExists annotation
			return successResult, nil, nil, nil
		}

		// For other errors, log and return as usual
		l.Error("Failed to create user in Grafana", zap.Error(err), zap.String("email", email))
		return nil, nil, nil, fmt.Errorf("grafana-connector: failed to create user: %w", err)
	}

	// Create a resource from the new user
	resource, err := userResource(user)
	if err != nil {
		l.Error("Failed to create resource for new user", zap.Error(err))
		return nil, nil, nil, fmt.Errorf("grafana-connector: failed to create resource for new user: %w", err)
	}

	// Return success result with the new user resource
	successResult := &v2.CreateAccountResponse_SuccessResult{
		Resource: resource,
	}

	// Return the password as plaintext data
	plaintextData := []*v2.PlaintextData{
		{
			Name:  "password",
			Bytes: []byte(password),
		},
	}

	return successResult, plaintextData, nil, nil
}

func (o *userBuilder) Delete(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	err := o.client.DeleteUser(ctx, resourceId.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to delete user: %w", err)
	}
	return nil, nil
}
