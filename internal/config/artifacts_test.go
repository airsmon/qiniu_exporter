package config

import (
	"encoding/json"
	"os"
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
	file, err := os.Open("../../grafana/qiniu_exporter.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()

	var dashboard struct {
		Title  string `json:"title"`
		Panels []struct {
			ID      int    `json:"id"`
			Type    string `json:"type"`
			Targets []struct {
				Expression string `json:"expr"`
			} `json:"targets"`
		} `json:"panels"`
	}
	if err := json.NewDecoder(file).Decode(&dashboard); err != nil {
		t.Fatal(err)
	}
	if dashboard.Title == "" || len(dashboard.Panels) == 0 {
		t.Fatal("Grafana dashboard title or panels are empty")
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
		}
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
