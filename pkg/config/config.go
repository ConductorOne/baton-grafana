package config

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

const (
	BasicAuthFieldGroup = "basic-auth-flow-group"
	ApiKeyFieldGroup    = "api-key-flow-group" //nolint:gosec // ApiKeyFieldGroup is the name of the field group for the API key flow.
)

var (
	Hostname = field.StringField("hostname",
		field.WithRequired(true),
		field.WithDisplayName("Instance URL"),
		field.WithDescription("The Grafana instance URL used to connect to the Grafana API."),
		field.WithPlaceholder("https://grafana.example.com"))

	Username = field.StringField("username",
		field.WithRequired(true),
		field.WithDisplayName("Username"),
		field.WithDescription("The Grafana username used to connect to the Grafana API."))

	Password = field.StringField("password",
		field.WithRequired(true),
		field.WithIsSecret(true),
		field.WithDisplayName("Password"),
		field.WithDescription("The Grafana password used to connect to the Grafana API."))

	APIToken = field.StringField("api-token",
		field.WithRequired(true),
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
		field.WithIconUrl("/static/app-icons/grafana.svg"),
		field.WithFieldGroups([]field.SchemaFieldGroup{
			{
				Name:        BasicAuthFieldGroup,
				DisplayName: "Basic Authentication",
				HelpText:    "Use the Basic Auth Flow for authentication.",
				Fields:      []field.SchemaField{Username, Password, Hostname},
				Default:     true,
			},
			{
				Name:        ApiKeyFieldGroup,
				DisplayName: "API Key",
				HelpText:    "Use an API Key with an expiration date for authentication.",
				Fields:      []field.SchemaField{APIToken, Hostname},
				Default:     false,
			},
		}),
	)
)
