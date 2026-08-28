package main

import (
	"context"

	cfg "github.com/conductorone/baton-grafana/pkg/config"
	"github.com/conductorone/baton-grafana/pkg/connector"
	"github.com/conductorone/baton-sdk/pkg/cli"
	"github.com/conductorone/baton-sdk/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/connectorrunner"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

var version = "dev"

func main() {
	ctx := context.Background()

	config.RunConnector(
		ctx,
		"baton-grafana",
		version,
		cfg.Config,
		getConnector,
		connectorrunner.WithDefaultCapabilitiesConnectorBuilderV2(&connector.Grafana{}),
	)
}

func getConnector(ctx context.Context, gc *cfg.Grafana, connectorOpts *cli.ConnectorOpts) (
	connectorbuilder.ConnectorBuilderV2, []connectorbuilder.Opt, error,
) {
	l := ctxzap.Extract(ctx)

	cb, err := connector.New(ctx, gc.Hostname, gc.Username, gc.Password, gc.APIToken, connectorOpts)
	if err != nil {
		l.Error("error creating connector", zap.Error(err))
		return nil, nil, err
	}

	return cb, nil, nil
}
