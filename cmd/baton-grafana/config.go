package main

import (
	"github.com/conductorone/baton-sdk/pkg/field"
	"github.com/spf13/viper"
)

var (
	AccessToken = field.StringField("access-token", field.WithDescription("The Grafana Personal Access Token used to connect to the Grafana API."))
	Username    = field.StringField("username", field.WithDescription("The Grafana username used to connect to the Grafana API."))
	Password    = field.StringField("password", field.WithDescription("The Grafana password used to connect to the Grafana API."))
	Orgs        = field.StringSliceField("orgs", field.WithDescription("Limit syncing to specific organizations by providing organization slugs."))
	// ConfigurationFields defines the external configuration required for the
	// connector to run. Note: these fields can be marked as optional or
	// required.
	ConfigurationFields = []field.SchemaField{
		Username,
		AccessToken,
		Password,
		Orgs,
	}

	// FieldRelationships defines relationships between the fields listed in
	// ConfigurationFields that can be automatically validated. For example, a
	// username and password can be required together, or an access token can be
	// marked as mutually exclusive from the username password pair.
	FieldRelationships = []field.SchemaFieldRelationship{
		field.FieldsAtLeastOneUsed(AccessToken, Password),

		field.FieldsMutuallyExclusive(AccessToken, Password),

		field.FieldsRequiredTogether(Username, Password),
	}

	cfg = field.Configuration{
		Fields:      ConfigurationFields,
		Constraints: FieldRelationships,
	}
)

// ValidateConfig is run after the configuration is loaded, and should return an
// error if it isn't valid. Implementing this function is optional, it only
// needs to perform extra validations that cannot be encoded with configuration
// parameters.
func ValidateConfig(v *viper.Viper) error {
	return nil
}
