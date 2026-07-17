package connector

import (
	"fmt"
	"strconv"

	v2 "github.com/conductorone/baton-sdk/pb/c1/connector/v2"
	"github.com/conductorone/baton-sdk/pkg/annotations"
	"github.com/conductorone/baton-sdk/pkg/pagination"
	"golang.org/x/text/cases"
	"golang.org/x/text/language"
)

const ResourcesPageSize uint64 = 50

func titleCase(s string) string {
	titleCaser := cases.Title(language.English)

	return titleCaser.String(s)
}

func annotationsForUserResourceType() annotations.Annotations {
	annos := annotations.Annotations{}
	annos.Update(&v2.SkipEntitlementsAndGrants{})
	return annos
}

// If pagToken.Token is an empty string, the function returns 1, the first page.
// Grafana's list endpoints are 1-based: page=1 is the first page and page=0 is
// treated as page one as well. Starting at 1 (rather than 0) avoids fetching the
// first page twice, since nextPage is derived as page+1 (CXH-2013).
func parsePageToken(pagToken *pagination.Token, resourceID *v2.ResourceId) (*pagination.Bag, uint64, error) {
	bag := &pagination.Bag{}
	err := bag.Unmarshal(pagToken.Token)
	if err != nil {
		return nil, 0, err
	}

	page := uint64(1)

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
