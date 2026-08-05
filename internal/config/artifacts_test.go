package config

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"go.yaml.in/yaml/v3"
)

func TestExampleConfigurationsAreValid(t *testing.T) {
	for _, path := range []string{
		"../../configs/qiniu-exporter.example.yaml",
		"../../configs/qiniu-exporter.compose.yaml",
	} {
		t.Run(path, func(t *testing.T) {
			cfg, err := Load(path)
			if err != nil {
				t.Fatal(err)
			}
			if cfg.Kodo.Credential != "main" || cfg.CDN.Credential != "main" || cfg.Billing.Credential != "main" {
				t.Fatal("example modules must share the main credential")
			}
		})
	}
}

func TestGrafanaDashboardHasPanelsAndQueries(t *testing.T) {
	data, err := os.ReadFile("../../grafana/qiniu_exporter.json")
	if err != nil {
		t.Fatal(err)
	}

	var dashboard struct {
		Title      string `json:"title"`
		Version    int    `json:"version"`
		Templating struct {
			List []struct {
				Name       string          `json:"name"`
				Definition string          `json:"definition"`
				AllValue   string          `json:"allValue"`
				Current    json.RawMessage `json:"current"`
				Multi      bool            `json:"multi"`
				IncludeAll bool            `json:"includeAll"`
			} `json:"list"`
		} `json:"templating"`
		Panels []struct {
			ID          int                        `json:"id"`
			Title       string                     `json:"title"`
			Type        string                     `json:"type"`
			Description string                     `json:"description"`
			GridPos     map[string]int             `json:"gridPos"`
			Options     map[string]json.RawMessage `json:"options"`
			FieldConfig struct {
				Defaults  map[string]json.RawMessage `json:"defaults"`
				Overrides json.RawMessage            `json:"overrides"`
			} `json:"fieldConfig"`
			Transformations json.RawMessage `json:"transformations"`
			Targets         []struct {
				Expression string `json:"expr"`
				Instant    bool   `json:"instant"`
				RefID      string `json:"refId"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.Unmarshal(data, &dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.Title == "" || len(dashboard.Panels) == 0 {
		t.Fatal("Grafana dashboard title or panels are empty")
	}
	if dashboard.Version < 8 {
		t.Fatalf("Grafana dashboard version = %d, want at least 8", dashboard.Version)
	}

	variables := make(map[string]int, len(dashboard.Templating.List))
	for index, variable := range dashboard.Templating.List {
		variables[variable.Name] = index
	}
	accountIndex, ok := variables["qiniu_account"]
	if !ok {
		t.Fatal("Grafana dashboard has no qiniu_account variable")
	}
	account := dashboard.Templating.List[accountIndex]
	if account.Multi || account.IncludeAll || strings.Contains(string(account.Current), "production") {
		t.Fatalf("qiniu_account must be single-select without a baked production value: %#v", account)
	}
	if jobIndex, ok := variables["job"]; !ok || accountIndex >= jobIndex {
		t.Fatal("qiniu_account must precede downstream job variables")
	}
	for name, metric := range map[string]string{
		"bucket":      "qiniu_kodo_bucket_info",
		"kodo_region": "qiniu_kodo_bucket_info",
		"domain":      "qiniu_cdn_domain_info",
		"cdn_region":  "qiniu_cdn_monitoring_bandwidth_bits_per_second",
		"currency":    "qiniu_billing_",
	} {
		index, ok := variables[name]
		if !ok {
			t.Fatalf("Grafana dashboard has no %s variable", name)
		}
		variable := dashboard.Templating.List[index]
		if !variable.Multi || !variable.IncludeAll || variable.AllValue != ".*" || !strings.Contains(string(variable.Current), "$__all") {
			t.Fatalf("Grafana variable %q must default to a regex All selection: %#v", name, variable)
		}
		if !strings.Contains(variable.Definition, metric) {
			t.Fatalf("Grafana variable %q is not sourced from %q: %s", name, metric, variable.Definition)
		}
	}
	if _, exists := variables["region"]; exists {
		t.Fatal("Kodo Region IDs and CDN traffic regions must not share one dashboard variable")
	}
	for _, variable := range dashboard.Templating.List {
		if variable.Name == "DS_PROMETHEUS" || variable.Name == "qiniu_account" {
			continue
		}
		if !strings.Contains(variable.Definition, "qiniu_account") {
			t.Fatalf("Grafana variable %q is not scoped by qiniu_account", variable.Name)
		}
	}

	ids := make(map[int]struct{}, len(dashboard.Panels))
	for _, panel := range dashboard.Panels {
		if _, exists := ids[panel.ID]; exists {
			t.Fatalf("duplicate Grafana panel id %d", panel.ID)
		}
		ids[panel.ID] = struct{}{}
		if panel.Type == "row" {
			continue
		}
		if len(panel.Targets) == 0 {
			t.Fatalf("Grafana panel %d has no query", panel.ID)
		}
		for _, target := range panel.Targets {
			if target.Expression == "" {
				t.Fatalf("Grafana panel %d has an empty PromQL expression", panel.ID)
			}
			if !strings.Contains(target.Expression, "qiniu_account") {
				t.Fatalf("Grafana panel %d query is not scoped by qiniu_account: %s", panel.ID, target.Expression)
			}
		}
	}
	for leftIndex, left := range dashboard.Panels {
		if left.Type == "row" {
			continue
		}
		for _, right := range dashboard.Panels[leftIndex+1:] {
			if right.Type == "row" {
				continue
			}
			leftX, leftY := left.GridPos["x"], left.GridPos["y"]
			rightX, rightY := right.GridPos["x"], right.GridPos["y"]
			if leftX < rightX+right.GridPos["w"] && rightX < leftX+left.GridPos["w"] &&
				leftY < rightY+right.GridPos["h"] && rightY < leftY+left.GridPos["h"] {
				t.Fatalf("Grafana panels %d and %d overlap: %#v %#v", left.ID, right.ID, left.GridPos, right.GridPos)
			}
		}
	}

	panelByID := func(id int) *struct {
		ID          int                        `json:"id"`
		Title       string                     `json:"title"`
		Type        string                     `json:"type"`
		Description string                     `json:"description"`
		GridPos     map[string]int             `json:"gridPos"`
		Options     map[string]json.RawMessage `json:"options"`
		FieldConfig struct {
			Defaults  map[string]json.RawMessage `json:"defaults"`
			Overrides json.RawMessage            `json:"overrides"`
		} `json:"fieldConfig"`
		Transformations json.RawMessage `json:"transformations"`
		Targets         []struct {
			Expression string `json:"expr"`
			Instant    bool   `json:"instant"`
			RefID      string `json:"refId"`
		} `json:"targets"`
	} {
		for index := range dashboard.Panels {
			if dashboard.Panels[index].ID == id {
				return &dashboard.Panels[index]
			}
		}
		t.Fatalf("Grafana panel %d is missing", id)
		return nil
	}

	target := panelByID(2).Targets[0].Expression
	if !strings.Contains(target, "max by (qiniu_account, job, instance)") || !strings.Contains(target, "up{qiniu_account") {
		t.Fatalf("Prometheus Target panel is not exporter-account scoped: %s", target)
	}
	rateLimits := panelByID(5).Targets[0].Expression
	for _, token := range []string{"0 *", "up{qiniu_account", "== 1", "on(qiniu_account)"} {
		if !strings.Contains(rateLimits, token) {
			t.Fatalf("rate-limit healthy-zero query is missing %q: %s", token, rateLimits)
		}
	}
	gates := panelByID(9)
	if len(gates.Targets) != 2 || !strings.Contains(gates.Targets[0].Expression, "timezone_unverified") || !strings.Contains(gates.Targets[0].Expression, "allowlist_empty") || !strings.Contains(gates.Targets[1].Expression, "increase(") {
		t.Fatalf("collection-gates panel does not distinguish persistent gates from recent skips: %#v", gates.Targets)
	}
	for id, token := range map[int]string{
		4:  "on(qiniu_account, job, instance, module, collector)",
		7:  "by (qiniu_account, service, result)",
		8:  "by (qiniu_account, service, host, le)",
		15: "by (qiniu_account, service, endpoint, le)",
		25: "on(qiniu_account, domain)",
	} {
		if !strings.Contains(panelByID(id).Targets[0].Expression, token) {
			t.Fatalf("Grafana panel %d does not preserve qiniu_account in aggregation or matching", id)
		}
	}
	if _, exists := ids[33]; exists {
		t.Fatal("obsolete Billing Periods panel must be removed")
	}
	for _, id := range []int{8, 11, 12, 13, 14, 15, 16, 21, 22, 23, 24, 25, 26, 27, 28, 31, 34, 35, 36, 39, 40, 41, 42, 43, 44, 45, 46, 47, 48, 49, 50, 51, 52, 53, 54, 55, 56} {
		if panelByID(id).Description == "" {
			t.Fatalf("Grafana panel %d must explain gated or disabled no-data semantics", id)
		}
	}
	for id, metric := range map[int]string{
		39: "qiniu_kodo_buckets",
		41: "qiniu_cdn_domains",
		42: "qiniu_cdn_domain_info",
		43: "qiniu_cdn_domain_info",
	} {
		panel := panelByID(id)
		if len(panel.Targets) != 1 || !panel.Targets[0].Instant || !strings.Contains(panel.Targets[0].Expression, metric) {
			t.Fatalf("inventory panel %d is invalid: %#v", id, panel)
		}
	}
	bucketInventory := panelByID(40)
	if len(bucketInventory.Targets) != 5 {
		t.Fatalf("Bucket Inventory targets = %d, want inventory plus four usage columns", len(bucketInventory.Targets))
	}
	for _, target := range bucketInventory.Targets {
		if !target.Instant {
			t.Fatalf("Bucket Inventory target %s must be instant: %#v", target.RefID, target)
		}
	}
	for _, token := range []string{"storage_region", "access", "max by (qiniu_account, bucket, storage_region, region, access)", "${kodo_region:regex}"} {
		if !strings.Contains(bucketInventory.Targets[0].Expression, token) {
			t.Fatalf("Bucket Inventory query is missing %q: %s", token, bucketInventory.Targets[0].Expression)
		}
	}
	for index, tokens := range [][]string{
		{"qiniu_kodo_storage_bytes", "storage_class", "sum by (qiniu_account, bucket, region)"},
		{"qiniu_kodo_objects", "storage_class", "sum by (qiniu_account, bucket, region)"},
		{"qiniu_kodo_usage_egress_bytes", `route="direct"`, `period="current_month"`},
		{"qiniu_kodo_usage_requests", `operation="put"`, `period="current_month"`},
	} {
		expression := bucketInventory.Targets[index+1].Expression
		for _, token := range tokens {
			if !strings.Contains(expression, token) {
				t.Fatalf("Bucket Inventory target %s is missing %q: %s", bucketInventory.Targets[index+1].RefID, token, expression)
			}
		}
	}
	bucketPresentation := string(bucketInventory.Transformations) + string(bucketInventory.FieldConfig.Overrides)
	for _, token := range []string{"Bucket", "Storage Region", "Region ID", "Access Control", "Today's Storage", "Today's Objects", "Month Direct Egress", "Month PUT Requests", "merge", "bytes", "public", "Public", "orange", "private", "Private", "green", "color-background"} {
		if !strings.Contains(bucketPresentation, token) {
			t.Fatalf("Bucket Inventory presentation is missing %q: %s", token, bucketPresentation)
		}
	}
	stateSummary := panelByID(42)
	if stateSummary.Type != "stat" || stateSummary.GridPos["x"] != 4 || stateSummary.GridPos["w"] != 8 || !strings.Contains(string(stateSummary.Options["colorMode"]), "background") {
		t.Fatalf("CDN state summary is not a semantic stat block: %#v", stateSummary)
	}
	stateColors := string(stateSummary.FieldConfig.Overrides)
	for _, token := range []string{"success", "green", "processing", "blue", "failed", "red", "frozen", "orange", "offlined", "gray"} {
		if !strings.Contains(stateColors, token) {
			t.Fatalf("CDN state summary color mapping is missing %q: %s", token, stateColors)
		}
	}
	stateTable := panelByID(43)
	if stateTable.GridPos["x"] != 12 || stateTable.GridPos["w"] != 12 || !strings.Contains(string(stateTable.FieldConfig.Overrides), "color-background") {
		t.Fatalf("CDN inventory state cells are not color blocks: %#v", stateTable)
	}
	for id, token := range map[int]string{
		47: `period="last_complete_hour"`,
		48: `period="last_complete_hour"`,
		49: `period="today"`,
		50: `period="today"`,
	} {
		panel := panelByID(id)
		if panel.Type != "stat" || panel.GridPos["y"] != 51 || panel.GridPos["h"] != 4 || panel.GridPos["w"] != 6 || len(panel.Targets) != 1 || !panel.Targets[0].Instant || !strings.Contains(panel.Targets[0].Expression, token) {
			t.Fatalf("CDN usage card %d is invalid: %#v", id, panel)
		}
	}
	for id, token := range map[int]string{
		51: `period="current_month"`,
		52: "qiniu_cdn_usage_active_domains",
		55: `period="current_month"`,
		56: "qiniu_cdn_domains",
	} {
		panel := panelByID(id)
		if panel.Type != "stat" || panel.GridPos["y"] != 55 || panel.GridPos["h"] != 4 || panel.GridPos["w"] != 6 || len(panel.Targets) != 1 || !panel.Targets[0].Instant || !strings.Contains(panel.Targets[0].Expression, token) {
			t.Fatalf("CDN second-row card %d is invalid: %#v", id, panel)
		}
	}
	for _, id := range []int{47, 49, 51} {
		panel := panelByID(id)
		query := panel.Targets[0].Expression
		if !strings.Contains(query, "qiniu_cdn_usage_account_traffic_bytes") || strings.Contains(query, "domain=~") || string(panel.FieldConfig.Defaults["unit"]) != `"decbytes"` {
			t.Fatalf("CDN account traffic card %d must be a complete, all-domain decimal-byte total: %#v", id, panel)
		}
	}
	for _, id := range []int{48, 50, 55} {
		panel := panelByID(id)
		query := panel.Targets[0].Expression
		if !strings.Contains(query, "qiniu_cdn_usage_account_peak_bandwidth_bits_per_second") || strings.Contains(query, "domain=~") || string(panel.FieldConfig.Defaults["unit"]) != `"bps"` {
			t.Fatalf("CDN account peak card %d must be a synchronized all-domain bits-per-second value: %#v", id, panel)
		}
	}
	if query := panelByID(52).Targets[0].Expression; strings.Contains(query, "domain=~") {
		t.Fatalf("active-domain account card must intentionally ignore the domain selector: %s", query)
	}
	for id, token := range map[int]string{
		53: "qiniu_cdn_usage_traffic_bytes",
		54: "qiniu_cdn_usage_peak_bandwidth_bits_per_second",
	} {
		panel := panelByID(id)
		if panel.Type != "bargauge" || panel.GridPos["y"] != 67 || panel.GridPos["h"] != 8 || panel.GridPos["w"] != 12 || len(panel.Targets) != 1 || !panel.Targets[0].Instant || !strings.Contains(panel.Targets[0].Expression, "topk(5") || !strings.Contains(panel.Targets[0].Expression, token) || !strings.Contains(panel.Targets[0].Expression, "qiniu_cdn_usage_complete") {
			t.Fatalf("CDN Top 5 panel %d is invalid: %#v", id, panel)
		}
	}
	if target := panelByID(54).Targets[0].Expression; !strings.Contains(target, `period="current_month"`) || !strings.Contains(target, "and on (qiniu_account, domain)") {
		t.Fatalf("monthly bandwidth panel is not aligned to the monthly traffic Top 5: %s", target)
	}
	for _, id := range []int{21, 22} {
		panel := panelByID(id)
		if panel.GridPos["y"] != 59 || !strings.Contains(panel.Targets[0].Expression, "sum by (qiniu_account, region)") || !strings.Contains(panel.Targets[0].Expression, "max by (qiniu_account, domain, region)") || !strings.Contains(panel.Targets[0].Expression, "${cdn_region:regex}") {
			t.Fatalf("CDN monitoring overview panel %d is not aggregated by selected domain scope: %#v", id, panel)
		}
	}
	for id, metric := range map[int]string{
		31: "qiniu_billing_available_balance",
		44: "qiniu_billing_unpaid_amount",
		45: "qiniu_billing_estimated_cost",
		46: "qiniu_billing_last_finalized_cost",
	} {
		card := panelByID(id)
		if card.Type != "stat" || card.GridPos["y"] != 100 || card.GridPos["h"] != 5 || card.GridPos["w"] != 6 || len(card.Targets) != 1 {
			t.Fatalf("Billing KPI card %d is invalid: %#v", id, card)
		}
		target := card.Targets[0]
		if !target.Instant || !strings.Contains(target.Expression, metric) || !strings.Contains(target.Expression, "max by (qiniu_account, currency)") {
			t.Fatalf("invalid Billing KPI card query: %#v", target)
		}
	}
	if _, exists := panelByID(31).FieldConfig.Defaults["thresholds"]; exists {
		t.Fatal("Available Balance must not assume an account-specific threshold")
	}
	if _, exists := panelByID(44).FieldConfig.Defaults["thresholds"]; !exists {
		t.Fatal("Unpaid Amount must distinguish zero from positive debt")
	}
	monthly := panelByID(37)
	if monthly.Type != "bargauge" || monthly.GridPos["y"] != 105 || monthly.GridPos["w"] != 24 || len(monthly.Targets) != 1 || !monthly.Targets[0].Instant || !strings.Contains(monthly.Targets[0].Expression, "qiniu_billing_current_year_monthly_finalized_cost") {
		t.Fatalf("current-year monthly Billing panel is invalid: %#v", monthly)
	}
	summary := panelByID(38)
	if summary.GridPos["y"] != 113 || summary.GridPos["w"] != 24 || summary.GridPos["h"] != 5 || len(summary.Targets) != 3 {
		t.Fatalf("current-year Billing summary target count = %d, want 3", len(summary.Targets))
	}
	for _, target := range summary.Targets {
		if !target.Instant || !strings.Contains(target.Expression, "qiniu_billing_current_year_monthly_finalized_cost") {
			t.Fatalf("invalid current-year Billing summary query: %#v", target)
		}
	}
	resourcePacks := panelByID(36)
	if len(resourcePacks.Targets) != 2 || !strings.Contains(resourcePacks.Targets[1].Expression, "allowlist_empty") {
		t.Fatal("resource-pack status does not distinguish disabled collection")
	}
}

func TestPrometheusRulesAreValidYAML(t *testing.T) {
	file, err := os.Open("../../rules/qiniu-exporter.rules.yml")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var document struct {
		Groups []struct {
			Name  string `yaml:"name"`
			Rules []struct {
				Alert       string            `yaml:"alert"`
				Record      string            `yaml:"record"`
				Expr        string            `yaml:"expr"`
				For         string            `yaml:"for"`
				Labels      map[string]string `yaml:"labels"`
				Annotations map[string]string `yaml:"annotations"`
			} `yaml:"rules"`
		} `yaml:"groups"`
	}
	decoder := yaml.NewDecoder(file)
	decoder.KnownFields(true)
	if err := decoder.Decode(&document); err != nil {
		t.Fatal(err)
	}
	if len(document.Groups) == 0 || len(document.Groups[0].Rules) == 0 {
		t.Fatal("rule file is empty")
	}
	for _, group := range document.Groups {
		for _, rule := range group.Rules {
			if rule.Expr == "" || (rule.Alert == "" && rule.Record == "") {
				t.Fatalf("invalid rule in group %q: %#v", group.Name, rule)
			}
		}
	}
}
