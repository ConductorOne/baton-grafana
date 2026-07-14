package connector

import (
	"context"
	"fmt"
	"io"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

// Grafana represents the Baton connector for Grafana.
type Grafana struct {
	client *grafana.Client
}

// ResourceSyncers returns a list of syncers for different resource types.
func (g *Grafana) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	return []connectorbuilder.ResourceSyncer{
		newOrgBuilder(g.client),
		newUserBuilder(g.client),
	}
}

// Asset is used to fetch an asset based on an AssetRef.
func (g *Grafana) Asset(ctx context.Context, asset *v2.AssetRef) (string, io.ReadCloser, error) {
	return "", nil, nil
}

// Metadata provides information about the Grafana connector.
func (g *Grafana) Metadata(ctx context.Context) (*v2.ConnectorMetadata, error) {
	return &v2.ConnectorMetadata{
		DisplayName: "Grafana",
		Description: "Connector syncing Grafana organizations and users to Baton",
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
				"login": {
					DisplayName: "Username",
					Required:    false,
					Description: "Username for login (email will be used if not provided)",
					Field: &v2.ConnectorAccountCreationSchema_Field_StringField{
						StringField: &v2.ConnectorAccountCreationSchema_StringField{},
					},
					Placeholder: "johndoe",
					Order:       3,
				},
				"org_id": {
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
func New(ctx context.Context, hostname, username, password, apiToken string) (*Grafana, error) {
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
