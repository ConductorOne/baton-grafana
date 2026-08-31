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
	"github.com/conductorone/baton-sdk/pkg/uhttp"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
	"google.golang.org/grpc/codes"
)

type userBuilder struct {
	resourceType *v2.ResourceType
	client       *grafana.Client
}

var _ connectorbuilder.AccountManagerV2 = &userBuilder{}

// ResourceType returns the Baton resource type for users.
func (u *userBuilder) ResourceType(_ context.Context) *v2.ResourceType {
	return resourceTypeUser
}

// userResource creates a Baton resource for a Grafana user.
//
// is_externally_synced surfaces Grafana's native isExternallySynced flag verbatim —
// "the user's org role is managed by an external IdP (role sync)" — and only when
// Grafana actually returns it:
//
//   - Cloud (org-users, /api/org/users): the endpoint always returns the flag, so
//     the field is present and mirrors Grafana's value exactly.
//   - Self-hosted (global /api/users): the endpoint omits the flag, so User.
//     IsExternallySynced is nil and the field is left off the profile entirely.
//
// The value is NOT derived from AuthLabels. AuthLabels ("the user authenticated via
// an external module") is a different concept and is true for every Grafana Cloud
// user (all carry authLabels:["grafana.com"]); OR-ing it in reported true for every
// Cloud user and defeated the field's purpose. auth_labels is surfaced separately.
func userResource(user *grafana.User) (*v2.Resource, error) {
	hasAuthLabels := len(user.AuthLabels) > 0
	profile := map[string]any{
		"full_name":     user.Name,
		profileKeyLogin: user.Login,
		"user_id":       user.ID,
		profileKeyEmail: user.Email,
	}
	// Surface the native flag only when Grafana returned it; a value we don't have
	// is omitted rather than derived from a different concept (AuthLabels).
	if user.IsExternallySynced != nil {
		profile["is_externally_synced"] = *user.IsExternallySynced
	}
	if hasAuthLabels {
		// "; " (not ",") because a Grafana auth label may itself contain a comma.
		profile["auth_labels"] = strings.Join(user.AuthLabels, "; ")
	}

	status := v2.Status_RESOURCE_STATUS_ENABLED
	if user.IsDisabled {
		status = v2.Status_RESOURCE_STATUS_DISABLED
	}

	return rs.NewUserResource(
		user.Login,
		resourceTypeUser,
		user.ID,
		[]rs.UserTraitOption{
			rs.WithEmail(user.Email, true),
		},
		rs.WithResourceProfile(profile),
		rs.WithResourceStatus(status, ""),
	)
}

// List fetches all users in Grafana. Cloud mode uses the organization bound to
// the service account; self-hosted users are global, so neither path needs a parent.
func (u *userBuilder) List(ctx context.Context, _ *v2.ResourceId, attrs rs.SyncOpAttrs) ([]*v2.Resource, *rs.SyncOpResults, error) {
	if u.client.IsCloud() {
		return u.listCloud(ctx)
	}
	return u.listSelfHosted(ctx, &attrs.PageToken)
}

// listCloud fetches all members of the current org via GET /api/org/users (no pagination).
// ID stability: UserByOrgResponse.ID (json:"userId") == User.ID — same numeric Grafana user ID.
func (u *userBuilder) listCloud(ctx context.Context) ([]*v2.Resource, *rs.SyncOpResults, error) {
	orgUsers, annos, err := u.client.ListCurrentOrgUsers(ctx)
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: cloud: failed to list current org users: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(orgUsers))
	for _, orgUser := range orgUsers {
		if orgUser == nil {
			continue
		}
		user := orgUser.ToUser() // ID preserved: UserByOrgResponse.ID (userId) → User.ID
		ur, err := userResource(&user)
		if err != nil {
			return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: cloud: failed to create user resource: %w", err)
		}
		resources = append(resources, ur)
	}

	// No pagination — endpoint returns all members in a single response
	return resources, &rs.SyncOpResults{Annotations: annos}, nil
}

// listSelfHosted pages GET /api/users, the global user list on a self-hosted instance.
func (u *userBuilder) listSelfHosted(ctx context.Context, pToken *pagination.Token) ([]*v2.Resource, *rs.SyncOpResults, error) {
	// Parse pagination token. If Token is an empty string, the function returns 0.
	bag, page, err := parsePageToken(pToken, &v2.ResourceId{ResourceType: resourceTypeUser.Id})
	if err != nil {
		return nil, nil, fmt.Errorf("grafana-connector: failed to parse page token: %w", err)
	}

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: page,
	}

	// Fetch users from Grafana. The client normalizes 1-based paging.
	users, nextPage, annos, err := u.client.ListUsers(ctx, &paginationOpts)
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to list users: %w", err)
	}

	next, err := bag.NextToken(nextPage)
	if err != nil {
		return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to generate next token: %w", err)
	}

	resources := make([]*v2.Resource, 0, len(users))

	// Convert users to resources
	for _, user := range users {
		if user == nil {
			continue
		}
		// Self-hosted global /api/users omits the native flag — is_externally_synced is omitted.
		ur, err := userResource(user)
		if err != nil {
			return nil, &rs.SyncOpResults{Annotations: annos}, fmt.Errorf("grafana-connector: failed to create resource for user %s: %w", user.Email, err)
		}
		resources = append(resources, ur)
	}

	return resources, &rs.SyncOpResults{NextPageToken: next, Annotations: annos}, nil
}

// Entitlements returns an empty list for users.
func (u *userBuilder) Entitlements(_ context.Context, _ *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Entitlement, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// Grants returns an empty list for users.
func (u *userBuilder) Grants(_ context.Context, _ *v2.Resource, _ rs.SyncOpAttrs) ([]*v2.Grant, *rs.SyncOpResults, error) {
	return nil, nil, nil
}

// newUserBuilder initializes a user resource type.
func newUserBuilder(client *grafana.Client) *userBuilder {
	return &userBuilder{
		resourceType: resourceTypeUser,
		client:       client,
	}
}

// CreateAccountCapabilityDetails reports Cloud invite vs self-hosted password options.
// A nil client (capabilities prototype) falls through to the self-hosted set.
func (u *userBuilder) CreateAccountCapabilityDetails(_ context.Context) (*v2.CredentialDetailsAccountProvisioning, annotations.Annotations, error) {
	if u.client != nil && u.client.IsCloud() {
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
	emailVal := accountInfo.Profile.GetFields()[profileFieldEmail]
	if emailVal == nil || emailVal.GetStringValue() == "" {
		return nil, nil, nil, fmt.Errorf("grafana-connector: email is required for account creation")
	}
	email := emailVal.GetStringValue()

	// Get full name from profile or use email as fallback
	name := email
	if nameVal := accountInfo.Profile.GetFields()[profileFieldFullName]; nameVal != nil && nameVal.GetStringValue() != "" {
		name = nameVal.GetStringValue()
	}

	if u.client.IsCloud() {
		return u.createAccountCloud(ctx, l, email, name)
	}

	// Use email as login if not provided
	login := email
	if loginVal := accountInfo.Profile.GetFields()[profileKeyLogin]; loginVal != nil && loginVal.GetStringValue() != "" {
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

	inviteResp, annos, err := u.client.InviteUserToOrg(ctx, inviteReq)
	if err != nil {
		// Grafana Cloud disables the basic login form by default, so instance-level
		// invites for brand-new external users are rejected. Surface an actionable
		// error: the connector can only add users that already exist in the instance;
		// creating new users requires SCIM or the basic login form.
		if errors.Is(err, grafana.ErrExternalUserLoginDisabled) {
			// This configuration prerequisite is terminal, while the wrapped
			// sentinel remains available to callers.
			return nil, nil, annos, uhttp.WrapErrors(
				codes.InvalidArgument,
				fmt.Sprintf(
					"grafana-connector: cloud: cannot provision new user %q because the instance's basic login form is disabled (the Grafana Cloud default); "+
						"the connector can only add users that already exist in this instance (provisioned via SSO/SCIM/grafana.com) — "+
						"to create new users, enable SCIM provisioning or the basic login form",
					email),
				err)
		}
		return nil, nil, annos, fmt.Errorf("grafana-connector: cloud: failed to invite user: %w", err)
	}

	l.Debug("Cloud mode: invite sent", zap.Bool("email_sent", inviteResp.EmailSent))

	// ActionRequiredResult — user must accept invite; no resource ID available until they do
	return &v2.CreateAccountResponse_ActionRequiredResult{}, nil, annos, nil
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
	user, annos, err := u.client.CreateUser(ctx, createUserReq)
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
					return nil, nil, annos, fmt.Errorf("grafana-connector: failed to create user and couldn't find existing user: %w", err)
				}
			}

			// We found the user, create a resource for them (self-hosted /api/users).
			resource, resourceErr := userResource(existingUser)
			if resourceErr != nil {
				l.Error("Failed to create resource for existing user", zap.Error(resourceErr))
				return nil, nil, annos, fmt.Errorf("grafana-connector: failed to create resource for existing user: %w", resourceErr)
			}

			successResult := &v2.CreateAccountResponse_SuccessResult{
				Resource: resource,
			}

			// Return with GrantAlreadyExists annotation
			return successResult, nil, annos, nil
		}

		// For other errors, log and return as usual
		l.Error("Failed to create user in Grafana", zap.Error(err), zap.String("email", email))
		return nil, nil, annos, fmt.Errorf("grafana-connector: failed to create user: %w", err)
	}

	// Create a resource from the new user (self-hosted CreateUser response).
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

	return successResult, plaintextData, annos, nil
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

	annos, err := u.client.RemoveCurrentOrgUser(ctx, userID)
	if err != nil {
		return annos, fmt.Errorf("grafana-connector: cloud: failed to remove user %d from org: %w", userID, err)
	}

	return annos, nil
}

// deleteSelfHosted is the original Delete logic for self-hosted Grafana — unchanged.
func (u *userBuilder) deleteSelfHosted(ctx context.Context, resourceId *v2.ResourceId) (annotations.Annotations, error) {
	annos, err := u.client.DeleteUser(ctx, resourceId.Resource)
	if err != nil {
		return annos, fmt.Errorf("grafana-connector: failed to delete user: %w", err)
	}
	return annos, nil
}
