package grafana

import (
	"context"
	"errors"
	"net/url"
)

const nativeDashboardVersion = "dashboard.grafana.app/v2"

// dashboardDocument keeps native resources intact rather than converting their
// v2 spec to the classic dashboard model. The APIs use different identity fields.
type dashboardDocument struct {
	content map[string]any
	native  bool
}

func parseDashboard(content map[string]any) (dashboardDocument, error) {
	document := dashboardDocument{content: content}
	if version, exists := content["apiVersion"]; exists {
		if version != nativeDashboardVersion || content["kind"] != "Dashboard" {
			return document, errors.New("native dashboards require dashboard.grafana.app/v2 and kind Dashboard")
		}
		document.native = true
		if document.metadata() == nil || document.spec() == nil {
			return document, errors.New("native dashboard needs metadata and spec objects")
		}
	}
	title, _ := document.spec()["title"].(string)
	if !resourceID.MatchString(document.uid()) || title == "" {
		return document, errors.New("dashboard needs a stable UID and nonempty title")
	}
	return document, nil
}

func (d dashboardDocument) metadata() map[string]any {
	metadata, _ := d.content["metadata"].(map[string]any)
	return metadata
}

func (d dashboardDocument) spec() map[string]any {
	if d.native {
		spec, _ := d.content["spec"].(map[string]any)
		return spec
	}
	return d.content
}

func (d dashboardDocument) uid() string {
	if d.native {
		name, _ := d.metadata()["name"].(string)
		return name
	}
	uid, _ := d.content["uid"].(string)
	return uid
}

func (d dashboardDocument) namespace() string {
	namespace, _ := d.metadata()["namespace"].(string)
	return namespace
}

func (d dashboardDocument) folder() string {
	annotations, _ := d.metadata()["annotations"].(map[string]any)
	folder, _ := annotations["grafana.app/folder"].(string)
	return folder
}

func (d dashboardDocument) collectionPath() string {
	return "/apis/" + nativeDashboardVersion + "/namespaces/" + url.PathEscape(d.namespace()) + "/dashboards"
}

func (d dashboardDocument) resourcePath() string {
	return d.collectionPath() + "/" + url.PathEscape(d.uid())
}

func (c *Client) readDashboard(ctx context.Context, desired dashboardDocument, missingOK bool) (map[string]any, string, bool, error) {
	if desired.native {
		var current map[string]any
		found, err := c.request(ctx, "GET", desired.resourcePath(), nil, &current, missingOK)
		if err != nil || !found {
			return current, "", found, err
		}
		document, err := parseDashboard(current)
		if err != nil {
			return nil, "", false, err
		}
		if document.namespace() != desired.namespace() || document.uid() != desired.uid() {
			return nil, "", false, errors.New("native dashboard identity differs from request")
		}
		if version, _ := document.metadata()["resourceVersion"].(string); version == "" {
			return nil, "", false, errors.New("native dashboard has no resourceVersion")
		}
		return current, document.folder(), true, nil
	}
	var current dashboardResponse
	found, err := c.request(ctx, "GET", "/api/dashboards/uid/"+url.PathEscape(desired.uid()), nil, &current, missingOK)
	return current.Dashboard, current.Meta.FolderUID, found, err
}
