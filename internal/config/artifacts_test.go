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
			ID          int    `json:"id"`
			Title       string `json:"title"`
			Type        string `json:"type"`
			Description string `json:"description"`
			FieldConfig struct {
				Defaults map[string]json.RawMessage `json:"defaults"`
			} `json:"fieldConfig"`
			Targets []struct {
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
	if dashboard.Version < 3 {
		t.Fatalf("Grafana dashboard version = %d, want at least 3", dashboard.Version)
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
		"bucket":   "qiniu_kodo_bucket_info",
		"domain":   "qiniu_cdn_domain_info",
		"currency": "qiniu_billing_",
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

	panelByID := func(id int) *struct {
		ID          int    `json:"id"`
		Title       string `json:"title"`
		Type        string `json:"type"`
		Description string `json:"description"`
		FieldConfig struct {
			Defaults map[string]json.RawMessage `json:"defaults"`
		} `json:"fieldConfig"`
		Targets []struct {
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
	if _, exists := panelByID(31).FieldConfig.Defaults["thresholds"]; exists {
		t.Fatal("balance and unpaid amount must not share generic thresholds")
	}
	for _, id := range []int{8, 11, 12, 13, 14, 15, 16, 21, 22, 23, 24, 25, 26, 27, 28, 31, 33, 34, 35, 36, 39, 40, 41, 42, 43} {
		if panelByID(id).Description == "" {
			t.Fatalf("Grafana panel %d must explain gated or disabled no-data semantics", id)
		}
	}
	for id, metric := range map[int]string{
		39: "qiniu_kodo_buckets",
		40: "qiniu_kodo_bucket_info",
		41: "qiniu_cdn_domains",
		42: "qiniu_cdn_domain_info",
		43: "qiniu_cdn_domain_info",
	} {
		panel := panelByID(id)
		if len(panel.Targets) != 1 || !panel.Targets[0].Instant || !strings.Contains(panel.Targets[0].Expression, metric) {
			t.Fatalf("inventory panel %d is invalid: %#v", id, panel)
		}
	}
	billingSnapshot := panelByID(31)
	if billingSnapshot.Type != "stat" || len(billingSnapshot.Targets) != 4 {
		t.Fatalf("Billing snapshot panel is invalid: %#v", billingSnapshot)
	}
	for _, target := range billingSnapshot.Targets {
		if !target.Instant || !strings.Contains(target.Expression, "max by (qiniu_account, currency)") {
			t.Fatalf("invalid Billing snapshot query: %#v", target)
		}
	}
	billingPeriods := panelByID(33)
	if billingPeriods.Type != "stat" || len(billingPeriods.Targets) != 3 {
		t.Fatalf("Billing periods panel is invalid: %#v", billingPeriods)
	}
	for _, target := range billingPeriods.Targets {
		if !target.Instant || !strings.Contains(target.Expression, "max by (qiniu_account)") || !strings.Contains(target.Expression, "_timestamp_seconds") {
			t.Fatalf("invalid Billing-period query: %#v", target)
		}
	}
	monthly := panelByID(37)
	if monthly.Type != "bargauge" || len(monthly.Targets) != 1 || !monthly.Targets[0].Instant || !strings.Contains(monthly.Targets[0].Expression, "qiniu_billing_current_year_monthly_finalized_cost") {
		t.Fatalf("current-year monthly Billing panel is invalid: %#v", monthly)
	}
	summary := panelByID(38)
	if len(summary.Targets) != 3 {
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
