package connector

import (
	"context"
	"fmt"
	"io"
	"sync/atomic"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/cli"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

// Grafana represents the Baton connector for Grafana.
type Grafana struct {
	client *grafana.Client
	// rolesListed is set when roleBuilder.List successfully runs in this sync.
	// teamBuilder consults it before emitting team→role grants so OptIn-off
	// tenants (where role List is never scheduled) cannot mint dangling role
	// entitlement references. WillSyncResourceType is not enough: C1 passes the
	// opted-in set per task, while ConnectorOpts only sees the local CLI flag.
	rolesListed atomic.Bool
}

// ResourceSyncers returns a list of syncers for different resource types.
func (g *Grafana) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	return []connectorbuilder.ResourceSyncer{
		newOrgBuilder(g.client),
		newUserBuilder(g.client),
		newTeamBuilder(g.client, g.rolesWereListed),
		newRoleBuilder(g.client, g.markRolesListed),
		newServiceAccountBuilder(g.client),
	}
}

func (g *Grafana) markRolesListed() {
	g.rolesListed.Store(true)
}

func (g *Grafana) rolesWereListed() bool {
	return g.rolesListed.Load()
}

// Asset is used to fetch an asset based on an AssetRef.
func (g *Grafana) Asset(ctx context.Context, asset *v2.AssetRef) (string, io.ReadCloser, error) {
	return "", nil, nil
}

// Metadata provides information about the Grafana connector.
func (g *Grafana) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{
		DisplayName: "Grafana",
		Description: "Grafana connector syncs organizations, users, teams, RBAC roles, and service accounts. " +
			"Supports account provisioning, organization role assignment, team membership, and RBAC role assignment.",
		AccountCreationSchema: &v2.ConnectorAccountCreationSchema{
			FieldMap: map[string]*v2.ConnectorAccountCreationSchema_Field{
				profileFieldFullName: {
					DisplayName: "Full Name",
					Required:    true,
					Description: "User's full name for display purposes",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "John Doe",
					Order:       1,
				},
				profileFieldEmail: {
					DisplayName: "Email",
					Required:    true,
					Description: "User's email address (used for login if username is not provided)",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "user@example.com",
					Order:       2,
				},
				profileKeyLogin: {
					DisplayName: "Username",
					Required:    false,
					Description: "Username for login (email will be used if not provided)",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "johndoe",
					Order:       3,
				},
				profileKeyOrgID: {
					DisplayName: "Organization ID",
					Required:    false,
					Description: "ID of the organization to add user to",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "1",
					Order:       4,
				},
			},
		},
	}, nil
}

// Validate ensures the connector is properly configured and has valid API credentials.
func (g *Grafana) Validate(ctx context.Context) (annotations.Annotations, error) {
	if g.client.IsCloud() {
		// Cloud mode: /api/orgs is forbidden; use the current-org endpoint instead
		_, err := g.client.GetCurrentOrg(ctx)
		if err != nil {
			return nil, fmt.Errorf("grafana-connector: validate: failed to get current org: %w", err)
		}
	} else {
		// Self-hosted mode: original validation via server-admin endpoint
		// /api/orgs is 0-based: page 0 is the first page.
		paginationOpts := grafana.PaginationVars{
			Size: 1,
			Page: 0,
		}
		_, _, err := g.client.ListOrganizations(ctx, &paginationOpts)
		if err != nil {
			return nil, fmt.Errorf("grafana-connector: validate: failed to list organizations: %w", err)
		}
	}

	return nil, nil
}

// New initializes a new instance of the Grafana connector.
// When apiToken is non-empty the connector operates in Cloud mode (Bearer auth, current-org scope).
// When apiToken is empty the connector operates in self-hosted mode (Basic auth, server-admin scope).
// opts is accepted for the DefineConfigurationV2 / RunConnector wiring; role sync gating uses
// rolesListed (set by roleBuilder.List) instead of WillSyncResourceType.
func New(ctx context.Context, hostname, username, password, apiToken string, _ *cli.ConnectorOpts) (*Grafana, error) {
	grafanaClient, err := grafana.NewClient(ctx, hostname, username, password, apiToken)
	if err != nil {
		l := ctxzap.Extract(ctx)
		l.Error("Error creating Grafana client", zap.Error(err))
		return nil, err
	}

	return &Grafana{
		client: grafanaClient,
	}, nil
}
