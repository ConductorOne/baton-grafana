package connector

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/conductorone/baton-grafana/pkg/grafana"
	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const (
	ResourcesPageSize uint64 = 1000

	// Org role entitlement slugs (also used as Grafana role strings).
	roleViewer = "Viewer"
	roleEditor = "Editor"
	roleAdmin  = "Admin"

	teamMemberEntitlement   = "member"
	roleAssignedEntitlement = "assigned"

	// Role `name` prefixes from GET /api/access-control/roles for IRM and the
	// legacy OnCall plugin. `name` is stable across stacks; `uid` is
	// instance-specific and is required by POST/DELETE team-role endpoints.
	irmRolePrefix    = "plugins:grafana-irm-app:"
	onCallRolePrefix = "plugins:grafana-oncall-app:"

	profileKeyUID         = "uid"
	profileKeyLogin       = "login"
	profileKeyOrgID       = "org_id"
	profileKeyRole        = "role"
	profileKeyName        = "name"
	profileKeyGroup       = "group"
	profileKeyDescription = "description"
	profileKeyGlobal      = "global"
	profileKeyTeamID      = "team_id"
	profileKeyMemberCount = "member_count"
	profileKeySAID        = "service_account_id"
	profileKeyTokens      = "tokens"
	profileKeyIsExternal  = "is_external"
	profileKeyIsDisabled  = "is_disabled"

	// Shared profile keys. CreateAccount schema fields must use the same strings
	// so submitted values reach CreateAccount; resource profiles (users, teams)
	// reuse email for the API email field.
	profileFieldEmail    = "email"
	profileFieldFullName = "full_name"
)

var userRoles = []string{roleViewer, roleEditor, roleAdmin}

func titleCase(s string) string {
	titleCaser := cases.Title(language.English)

	return titleCaser.String(s)
}

func annotationsForUserResourceType() annotations.Annotations {
	annos := annotations.Annotations{}
	annos.Update(&v2.SkipEntitlementsAndGrants{})
	return annos
}

// If pagToken.Token is an empty string, the function returns 0.
// Callers decide what "page 0" means for their endpoint: /api/orgs is 0-based
// (page 0 is the first page), while /api/users is 1-based and normalizes the
// first page to 1.
func parsePageToken(pagToken *pagination.Token, resourceID *v2.ResourceId) (*pagination.Bag, uint64, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(pagToken.Token)
	if err != nil {
		return nil, 0, err
	}

	var page uint64

	if bag.Current() == nil {
		// If no current page state, push a new one for the provided resource.
		bag.Push(pagination.PageState{
			ResourceTypeID: resourceID.ResourceType,
			ResourceID:     resourceID.Resource,
		})
	} else if bag.Current().Token != "" {
		p, err := strconv.ParseUint(bag.Current().Token, 10, 32)
		if err != nil {
			return nil, 0, fmt.Errorf(
				"grafana-connector: failed to parse page token for resource type '%s' id '%s': %w (pageToken: %q)",
				resourceID.ResourceType,
				resourceID.Resource,
				err,
				bag.PageToken(),
			)
		}
		page = p
	}

	return bag, page, nil
}

func isIRMOrOnCallRole(name string) bool {
	return strings.HasPrefix(name, irmRolePrefix) || strings.HasPrefix(name, onCallRolePrefix)
}

// shouldEmitRole mirrors the List filter so team→role grants never target a
// role resource that List would skip (Hidden or non-IRM/OnCall).
func shouldEmitRole(role grafana.Role) bool {
	return !role.Hidden && isIRMOrOnCallRole(role.Name)
}
