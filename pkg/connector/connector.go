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
	orgs   []string
}

// ResourceSyncers returns a list of syncers for different resource types.
func (g *Grafana) ResourceSyncers(ctx context.Context) []connectorbuilder.ResourceSyncer {
	return []connectorbuilder.ResourceSyncer{
		orgBuilder(g.client, g.orgs),
		userBuilder(g.client),
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
		Description: "Connector syncing Grafana organizations, dashboards, teams, and users to Baton",
	}, nil
}

// Validate ensures the connector is properly configured and has valid API credentials.
func (g *Grafana) Validate(ctx context.Context) (annotations.Annotations, error) {

	paginationOpts := grafana.PaginationVars{
		Size: ResourcesPageSize,
		Page: 1,
	}

	// get the scope of used credentials
	_, _, err := g.client.ListOrganizations(ctx, &paginationOpts)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: validate: failed to list organizations: %w", err)
	}

	return nil, nil
}

// New initializes a new instance of the Grafana connector.
func New(ctx context.Context, username, accessToken, password string, orgs []string) (*Grafana, error) {
	l := ctxzap.Extract(ctx)
	l.Debug("creating Grafana client")

	grafanaClient, err := grafana.NewClient(ctx, username, password, accessToken)
	if err != nil {
		l.Error("error creating Grafana client", zap.Error(err))
		return nil, err
	}

	err = grafanaClient.SetCurrentUser(ctx, username)
	if err != nil {
		l.Error("error setting current user", zap.Error(err))
		return nil, err
	}

	// if len(orgs) == 0 {
	// 	paginationOpts := grafana.PaginationVars{
	// 		Size: ResourcesPageSize,
	// 		Page: 1,
	// 	}

	// 	currentUserOrgs, _, err := grafanaClient.ListOrganizations(ctx, &paginationOpts)

	// 	if err == nil {
	// 		orgsIDs := make([]string, len(currentUserOrgs))
	// 		for i, s := range currentUserOrgs {
	// 			orgsIDs[i] = fmt.Sprintf("%d", s.ID) // Convert int to string
	// 		}
	// 		orgs = append(orgs, orgsIDs...)
	// 	}
	// }

	return &Grafana{
		client: grafanaClient,
		orgs:   orgs,
	}, nil
}
