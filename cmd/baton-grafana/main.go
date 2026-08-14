package main

import (
	"context"
	"fmt"
	"os"

	cfg "github.com/conductorone/baton-grafana/pkg/config"
	"github.com/conductorone/baton-grafana/pkg/connector"
	"github.com/conductorone/baton-sdk/pkg/cli"
	configschema "github.com/conductorone/baton-sdk/pkg/config"
	"github.com/conductorone/baton-sdk/pkg/connectorbuilder"
	"github.com/conductorone/baton-sdk/pkg/types"
	"github.com/grpc-ecosystem/go-grpc-middleware/logging/zap/ctxzap"
	"go.uber.org/zap"
)

var version = "dev"

func main() {
	ctx := context.Background()

	_, cmd, err := configschema.DefineConfigurationV2(ctx, "baton-grafana", getConnector, cfg.Config)
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}

	cmd.Version = version

	err = cmd.Execute()
	if err != nil {
		fmt.Fprintln(os.Stderr, err.Error())
		os.Exit(1)
	}
}

func getConnector(ctx context.Context, gc *cfg.Grafana, runTimeOpts cli.RunTimeOpts) (types.ConnectorServer, error) {
	l := ctxzap.Extract(ctx)

	cb, err := connector.New(ctx, gc.Hostname, gc.Username, gc.Password, gc.APIToken, &cli.ConnectorOpts{
		TokenSource:         runTimeOpts.TokenSource,
		SelectedAuthMethod:  runTimeOpts.SelectedAuthMethod,
		SyncResourceTypeIDs: runTimeOpts.SyncResourceTypeIDs,
	})
	if err != nil {
		l.Error("error creating connector", zap.Error(err))
		return nil, err
	}

	c, err := connectorbuilder.NewConnector(ctx, cb)
	if err != nil {
		l.Error("error creating connector", zap.Error(err))
		return nil, err
	}

	return c, nil
}
