package config

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

var (
	Hostname = field.StringField("hostname",
		field.WithRequired(true),
		field.WithDisplayName("Instance URL"),
		field.WithDescription("The Grafana instance URL used to connect to the Grafana API."),
		field.WithPlaceholder("https://grafana.example.com"))
	Username = field.StringField("username",
		field.WithRequired(false),
		field.WithDisplayName("Username"),
		field.WithDescription("The Grafana username used to connect to the Grafana API."))
	Password = field.StringField("password",
		field.WithRequired(false),
		field.WithIsSecret(true),
		field.WithDisplayName("Password"),
		field.WithDescription("The Grafana password used to connect to the Grafana API."))
	APIToken = field.StringField("api-token",
		field.WithRequired(false),
		field.WithIsSecret(true),
		field.WithDisplayName("API Token"),
		field.WithDescription("Grafana Cloud service account token. When set, the connector uses Bearer authentication (Cloud mode). Leave empty for self-hosted Grafana."))

	//go:generate go run ./gen
	Config = field.NewConfiguration(
		[]field.SchemaField{
			Hostname,
			Username,
			Password,
			APIToken,
		},
		field.WithConnectorDisplayName("Grafana"),
		field.WithHelpUrl("/docs/baton/grafana"),
		field.WithIconUrl("/static/app-icons/grafana.svg"))
)
