package connector

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"

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
func (u *userBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeUser
}

// userResource creates a Baton resource for a Grafana user.
func userResource(user *grafana.User) (*v2.Resource, error) {
	// is_externally_synced surfaces the user's access origin, and its exact meaning
	// depends on which endpoint fed this user (the conflation is intentional):
	//   - Cloud (org-users): the native IsExternallySynced flag = the org role is
	//     managed by an external IdP.
	//   - Self-hosted (global /api/users): that flag is never returned, so we fall
	//     back to AuthLabels = the user authenticated via an external module
	//     (SSO/LDAP/OAuth). This is broader than role-sync, so a locally-managed
	//     role logging in via SSO also reports true.
	// Both cases answer "is this access externally originated?", which is the intent.
	hasAuthLabels := len(user.AuthLabels) > 0
	externallySynced := user.IsExternallySynced || hasAuthLabels
	profile := map[string]interface{}{
		"full_name":            user.Name,
		"login":                user.Login,
		"user_id":              user.ID,
		"email":                user.Email,
		"is_externally_synced": externallySynced,
	}
	if hasAuthLabels {
		// "; " (not ",") because a Grafana auth label may itself contain a comma.
		profile["auth_labels"] = strings.Join(user.AuthLabels, "; ")
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
// The parentResourceID parameter (SDK convention) is not used in either mode:
// in Cloud mode the connector operates on the single org bound to the service account,
// so no org scoping is needed; in self-hosted mode users are global Grafana entities,
// not scoped per org.
func (u *userBuilder) List(ctx context.Context, _ *v2.ResourceId, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
	if u.client.IsCloud() {
		return u.listCloud(ctx)
	}
	return u.listSelfHosted(ctx, pToken)
}

// listCloud fetches all members of the current org via GET /api/org/users (no pagination).
// ID stability: UserByOrgResponse.ID (json:"userId") == User.ID — same numeric Grafana user ID.
func (u *userBuilder) listCloud(ctx context.Context) ([]*v2.Resource, string, annotations.Annotations, error) {
	orgUsers, err := u.client.ListCurrentOrgUsers(ctx)
	if err != nil {
		return nil, "", nil, fmt.Errorf("grafana-connector: cloud: failed to list current org users: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(orgUsers))
	for _, orgUser := range orgUsers {
		user := orgUser.ToUser() // ID preserved: UserByOrgResponse.ID (userId) → User.ID
		ur, err := userResource(&user)
		if err != nil {
			return nil, "", nil, fmt.Errorf("grafana-connector: cloud: failed to create user resource: %w", err)
		}
		resources = append(resources, ur)
	}

	// No pagination — endpoint returns all members in a single response
	return resources, "", nil, nil
}

// listSelfHosted is the original List logic for self-hosted Grafana — unchanged.
func (u *userBuilder) listSelfHosted(ctx context.Context, pToken *pagination.Token) ([]*v2.Resource, string, annotations.Annotations, error) {
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
func (u *userBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Entitlement, string, annotations.Annotations, error) {
	return nil, "", nil, nil
}

// Grants returns an empty list for users.
func (u *userBuilder) Grants(_ context.Context, _ *v2.Resource, _ *pagination.Token) ([]*v2.Grant, string, annotations.Annotations, error) {
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
func (u *userBuilder) CreateAccountCapabilityDetails(_ context.Context) (*v2.CredentialDetailsAccountProvisioning, annotations.Annotations, error) {
	if u.client.IsCloud() {
		// Cloud mode: user creation is via org invite — no connector-generated password
		return &v2.CredentialDetailsAccountProvisioning{
			SupportedCredentialOptions: []v2.CapabilityDetailCredentialOption{
				v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_NO_PASSWORD,
			},
			PreferredCredentialOption: v2.CapabilityDetailCredentialOption_CAPABILITY_DETAIL_CREDENTIAL_OPTION_NO_PASSWORD,
		}, nil, nil
	}

	// Self-hosted mode: original behavior unchanged
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
	l := ctxzap.Extract(ctx)

	// Extract user information from profile — common to both modes
	emailVal := accountInfo.Profile.GetFields()["email"]
	if emailVal == nil || emailVal.GetStringValue() == "" {
		return nil, nil, nil, fmt.Errorf("grafana-connector: email is required for account creation")
	}
	email := emailVal.GetStringValue()

	// Get full name from profile or use email as fallback
	name := email
	if nameVal := accountInfo.Profile.GetFields()["full_name"]; nameVal != nil && nameVal.GetStringValue() != "" {
		name = nameVal.GetStringValue()
	}

	if u.client.IsCloud() {
		return u.createAccountCloud(ctx, l, email, name)
	}

	// Use email as login if not provided
	login := email
	if loginVal := accountInfo.Profile.GetFields()["login"]; loginVal != nil && loginVal.GetStringValue() != "" {
		login = loginVal.GetStringValue()
	}
	return u.createAccountSelfHosted(ctx, l, accountInfo, email, name, login, credentialOptions)
}

// createAccountCloud sends an invitation to the user via POST /api/org/invites.
//
// Cloud mode trade-offs vs self-hosted:
//   - No password is generated or returned (Grafana Cloud sets passwords via invite link).
//   - The user is not immediately active — they must accept the invitation.
//   - Returns ActionRequiredResult to signal the invite-pending state.
func (u *userBuilder) createAccountCloud(
	ctx context.Context,
	l *zap.Logger,
	email, name string,
) (connectorbuilder.CreateAccountResponse, []*v2.PlaintextData, annotations.Annotations, error) {
	l.Debug("Cloud mode: sending org invite")

	inviteReq := &grafana.InviteUserRequest{
		LoginOrEmail: email,
		Name:         name,
		Role:         roleViewer, // least-privileged default
		SendEmail:    true,
	}

	inviteResp, err := u.client.InviteUserToOrg(ctx, inviteReq)
	if err != nil {
		return nil, nil, nil, fmt.Errorf("grafana-connector: cloud: failed to invite user: %w", err)
	}

	l.Debug("Cloud mode: invite sent", zap.Bool("email_sent", inviteResp.EmailSent))

	// ActionRequiredResult — user must accept invite; no resource ID available until they do
	return &v2.CreateAccountResponse_ActionRequiredResult{}, nil, nil, nil
}

// createAccountSelfHosted is the original CreateAccount logic for self-hosted Grafana — unchanged.
func (u *userBuilder) createAccountSelfHosted(
	ctx context.Context,
	l *zap.Logger,
	accountInfo *v2.AccountInfo,
	email, name, login string,
	credentialOptions *v2.LocalCredentialOptions,
) (connectorbuilder.CreateAccountResponse, []*v2.PlaintextData, annotations.Annotations, error) {
	// Check if an organization ID was provided
	var orgId int
	if orgIdVal := accountInfo.Profile.GetFields()["org_id"]; orgIdVal != nil && orgIdVal.GetStringValue() != "" {
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
			l.Debug("User already exists in Grafana", zap.String("email", email), zap.String("login", login))

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

// Delete removes a user from Grafana.
func (u *userBuilder) Delete(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	if u.client.IsCloud() {
		return u.deleteCloud(ctx, resourceId)
	}
	return u.deleteSelfHosted(ctx, resourceId)
}

// deleteCloud removes a user from the current org (Cloud mode).
// LIMITATION: Grafana Cloud has no server-admin "delete user globally" endpoint accessible
// via a service account token. This operation removes the user from the org only — it does
// NOT delete the global Grafana Cloud account. A warning is logged to make this clear.
func (u *userBuilder) deleteCloud(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	l := ctxzap.Extract(ctx)

	userID, err := strconv.Atoi(resourceId.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: cloud: invalid user ID %s: %w", resourceId.Resource, err)
	}

	l.Debug("Cloud mode: delete removes user from org only — global Grafana Cloud account is NOT deleted",
		zap.String("user_id", resourceId.Resource))

	if err = u.client.RemoveCurrentOrgUser(ctx, userID); err != nil {
		return nil, fmt.Errorf("grafana-connector: cloud: failed to remove user %d from org: %w", userID, err)
	}

	return nil, nil
}

// deleteSelfHosted is the original Delete logic for self-hosted Grafana — unchanged.
func (u *userBuilder) deleteSelfHosted(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	err := u.client.DeleteUser(ctx, resourceId.Resource)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: failed to delete user: %w", err)
	}
	return nil, nil
}
