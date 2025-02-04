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

// If pagToken is an empty string, the function returns 0,
// as page 0 is considered the first page.
func parsePageToken(pagToken string, resourceID *v2.ResourceId) (*pagination.Bag, uint64, error) {
	b := &pagination.Bag{}
	err := b.Unmarshal(pagToken)
	if err != nil {
		return nil, 0, err
	}

	var page uint64 = 0

	if b.Current() != nil && b.Current().Token != "" {
		var err error
		page, err = strconv.ParseUint(b.Current().Token, 10, 32)
		if err != nil {
			return nil, 0, fmt.Errorf("grafana-connector: failed to convert string to uint %s: %w pageToke:%v", resourceID, err, b.PageToken())
		}
	}

	if b.Current() == nil {
		b.Push(pagination.PageState{
			ResourceTypeID: resourceID.ResourceType,
			ResourceID:     resourceID.Resource,
		})
	}

	return b, page, nil
}
