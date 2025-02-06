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
		orgBuilder(g.client),
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
		Description: "Connector syncing Grafana organizations and users to Baton",
	}, nil
}

// Validate ensures the connector is properly configured and has valid API credentials.
func (g *Grafana) Validate(ctx context.Context) (annotations.Annotations, error) {
	paginationOpts := grafana.PaginationVars{
		Size: 1,
		Page: 0,
	}

	// Get the scope of used credentials
	_, _, err := g.client.ListOrganizations(ctx, &paginationOpts)
	if err != nil {
		return nil, fmt.Errorf("grafana-connector: validate: failed to list organizations: %w", err)
	}

	return nil, nil
}

// New initializes a new instance of the Grafana connector.
func New(ctx context.Context, hostname, protocol, username, password string) (*Grafana, error) {
	l := ctxzap.Extract(ctx)
	l.Debug("creating Grafana client")

	grafanaClient, err := grafana.NewClient(ctx, hostname, protocol, username, password)
	if err != nil {
		l.Error("error creating Grafana client", zap.Error(err))
		return nil, err
	}

	return &Grafana{
		client: grafanaClient,
	}, nil
}
