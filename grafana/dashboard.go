package grafana

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"net/url"
	"strings"
	"time"

	"github.com/grafana/grafana-foundation-sdk/go/cog"
	"github.com/grafana/grafana-foundation-sdk/go/common"
	"github.com/grafana/grafana-foundation-sdk/go/dashboard"
	prom "github.com/grafana/grafana-foundation-sdk/go/prometheus"
	"github.com/grafana/grafana-foundation-sdk/go/stat"
	"github.com/grafana/grafana-foundation-sdk/go/timeseries"
	"github.com/owenyeo/go-observe/slo"
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

func overview(def Definition, target Target) (json.RawMessage, error) {
	ds := common.DataSourceRef{Type: cog.ToPtr("prometheus"), Uid: cog.ToPtr(target.PrometheusUID)}
	uid := def.OverviewUID
	if uid == "" {
		uid = NamespacedResourceUID(def.Namespace, def.Service, target.Environment, "dashboard", "overview")
	}
	title := def.Service + " — " + target.Environment
	if def.Namespace != "" {
		title = def.Namespace + " / " + title
	}
	builder := dashboard.NewDashboardBuilder(title).
		Uid(uid).Tags([]string{"go-observe:" + owner(def.Namespace, def.Service, target.Environment)}).
		Description("Service reliability. Unknown or no-traffic measurements remain No data. Monitoring release "+def.Version).
		Timezone("browser").Time("now-1h", "now").Refresh("30s")
	id := uint32(1)
	y := uint32(0)
	for _, objective := range def.Objectives {
		_, _, ratio, err := objective.SLI.Queries(objective.Window, target.Scope)
		if err != nil {
			return nil, err
		}
		builder.WithPanel(stat.NewPanelBuilder().Id(id).Title(objective.Name + " — success").Description(objective.Description + fmt.Sprintf(" Target %.4g%% over %s.", 100*objective.Target, objective.Window)).
			GridPos(dashboard.GridPos{X: 0, Y: y, W: 12, H: 4}).Datasource(ds).Unit("percentunit").NoValue("No data / no traffic").
			Thresholds(dashboard.NewThresholdsConfigBuilder().Mode(dashboard.ThresholdsModeAbsolute).Steps([]dashboard.Threshold{{Color: "red"}, {Color: "green", Value: cog.ToPtr(objective.Target)}})).
			WithTarget(prom.NewDataqueryBuilder().RefId("A").Expr(ratio).Instant()))
		builder.WithPanel(stat.NewPanelBuilder().Id(id + 1).Title(objective.Name + " — budget remaining").Description("Negative values mean the error budget has been exceeded.").
			GridPos(dashboard.GridPos{X: 12, Y: y, W: 12, H: 4}).Datasource(ds).Unit("percentunit").NoValue("No data / no traffic").
			Thresholds(dashboard.NewThresholdsConfigBuilder().Mode(dashboard.ThresholdsModeAbsolute).Steps([]dashboard.Threshold{{Color: "red"}, {Color: "orange", Value: cog.ToPtr(0.0)}, {Color: "green", Value: cog.ToPtr(.25)}})).
			WithTarget(prom.NewDataqueryBuilder().RefId("A").Expr(fmt.Sprintf("1 - ((1 - %s) / %.17g)", ratio, 1-objective.Target)).Instant()))
		id += 2
		y += 4
	}
	row := dashboard.NewRowBuilder("Diagnostics — traffic and errors").Id(id).GridPos(dashboard.GridPos{X: 0, Y: y, W: 24, H: 1})
	id++
	diagnosticScope := map[string]string{}
	for key, value := range target.Scope {
		diagnosticScope[key] = value
	}
	if def.ReplicaLabel != "" {
		if _, exists := target.Scope[def.ReplicaLabel]; exists {
			return nil, errors.New("replica label cannot also be fixed in target scope")
		}
		for _, objective := range def.Objectives {
			for _, selector := range []slo.Selector{objective.SLI.Total, objective.SLI.Good} {
				if _, exists := selector.Labels[def.ReplicaLabel]; exists {
					return nil, errors.New("replica label cannot also be fixed in an SLI selector")
				}
				if _, exists := selector.Regex[def.ReplicaLabel]; exists {
					return nil, errors.New("replica label cannot also be matched in an SLI selector")
				}
			}
		}
		series, err := def.Objectives[0].SLI.Total.Expression(target.Scope)
		if err != nil {
			return nil, err
		}
		// Validate a label using the same selector validator before using it in a variable query.
		if _, err := (slo.Selector{Metric: "scope", Labels: map[string]string{def.ReplicaLabel: ""}}).Expression(nil); err != nil {
			return nil, err
		}
		query := "label_values(" + series + ", " + def.ReplicaLabel + ")"
		builder.WithVariable(dashboard.NewQueryVariableBuilder("replica").Label("Replica (diagnostics)").Datasource(ds).
			Query(dashboard.StringOrMap{String: &query}).Multi(true).IncludeAll(true).AllValue(".*").AllowCustomValue(false).
			Refresh(dashboard.VariableRefreshOnDashboardLoad).Current(dashboard.VariableOption{Text: dashboard.StringOrArrayOfString{String: cog.ToPtr("All")}, Value: dashboard.StringOrArrayOfString{String: cog.ToPtr("$__all")}}))
	}
	for _, objective := range def.Objectives {
		diagnosticSLI := objective.SLI
		if def.ReplicaLabel != "" {
			// Add the matcher to structured selectors first. Rewriting every opening
			// brace would also corrupt route values such as /orders/{id}.
			diagnosticSLI.Total.Regex = maps.Clone(diagnosticSLI.Total.Regex)
			diagnosticSLI.Good.Regex = maps.Clone(diagnosticSLI.Good.Regex)
			if diagnosticSLI.Total.Regex == nil {
				diagnosticSLI.Total.Regex = map[string]string{}
			}
			if diagnosticSLI.Good.Regex == nil {
				diagnosticSLI.Good.Regex = map[string]string{}
			}
			diagnosticSLI.Total.Regex[def.ReplicaLabel] = ".*"
			diagnosticSLI.Good.Regex[def.ReplicaLabel] = ".*"
		}
		total, _, ratio, err := diagnosticSLI.Queries(5*time.Minute, diagnosticScope)
		if err != nil {
			return nil, err
		}
		if def.ReplicaLabel != "" {
			// The replica selector is only for drill-down panels. SLO totals and alerts
			// always use the full service population, regardless of UI selection.
			matcher := def.ReplicaLabel + `=~".*"`
			filter := def.ReplicaLabel + `=~"${replica:regex}"`
			total = strings.ReplaceAll(total, matcher, filter)
			ratio = strings.ReplaceAll(ratio, matcher, filter)
		}
		row.WithPanel(timeseries.NewPanelBuilder().Id(id).Title(objective.Name + " — eligible events/s").GridPos(dashboard.GridPos{X: 0, Y: y + 1, W: 12, H: 7}).Datasource(ds).Unit("ops").WithTarget(prom.NewDataqueryBuilder().RefId("A").Expr("(" + total + ") / 300").Range()))
		row.WithPanel(timeseries.NewPanelBuilder().Id(id + 1).Title(objective.Name + " — error fraction").GridPos(dashboard.GridPos{X: 12, Y: y + 1, W: 12, H: 7}).Datasource(ds).Unit("percentunit").NoValue("No data / no traffic").WithTarget(prom.NewDataqueryBuilder().RefId("A").Expr("1 - " + ratio).Range()))
		id += 2
		y += 7
	}
	builder.WithRow(row)
	dash, err := builder.Build()
	if err != nil {
		return nil, err
	}
	return json.Marshal(dash)
}

// Validate also runs on artifacts read from disk, before any network request.
