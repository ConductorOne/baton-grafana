package config

import (
	"github.com/conductorone/baton-sdk/pkg/field"
)

var (
	Username    = field.StringField("username", field.WithRequired(true), field.WithDescription("The Grafana username used to connect to the Grafana API."))
	AccessToken = field.StringField("access-token", field.WithDescription("The Grafana Personal Access Token used to connect to the Grafana API."))
	Password    = field.StringField("password", field.WithDescription("The Grafana password used to connect to the Grafana API."))
	Orgs        = field.StringSliceField("orgs", field.WithDescription("Limit syncing to specific organizations by providing organization slugs."))
)

var constraints = []field.SchemaFieldRelationship{
	field.FieldsMutuallyExclusive(AccessToken, Password),
	field.FieldsAtLeastOneUsed(AccessToken, Password),
}

var Configuration = field.NewConfiguration([]field.SchemaField{
	Username,
	AccessToken,
	Password,
	Orgs,
}, constraints...)
