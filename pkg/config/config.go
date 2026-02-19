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
		field.WithRequired(true),
		field.WithDisplayName("Username"),
		field.WithDescription("The Grafana username used to connect to the Grafana API."))
	Password = field.StringField("password",
		field.WithRequired(true),
		field.WithIsSecret(true),
		field.WithDisplayName("Password"),
		field.WithDescription("The Grafana password used to connect to the Grafana API."))

	//go:generate go run ./gen
	Config = field.NewConfiguration(
		[]field.SchemaField{
			Hostname,
			Username,
			Password,
		},
		field.WithConnectorDisplayName("Grafana"),
		field.WithHelpUrl("/docs/baton/grafana"),
		field.WithIconUrl("/static/app-icons/grafana.svg"))
)
