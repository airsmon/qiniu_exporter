package main

import (
	"math"
	"net/http"
	"reflect"
	"testing"

	"qiniu-exporter/internal/authhttp"
)

func TestSplitAttemptBudgetsUsesSmallestHardLimit(t *testing.T) {
	firstQPS, retryQPS := splitAttemptBudgets(0.5, 10, 0.1, 1)
	if math.Abs(firstQPS-0.05) > 1e-12 || math.Abs(retryQPS-0.02) > 1e-12 {
		t.Fatalf("first=%v retry=%v, want 0.05/0.02", firstQPS, retryQPS)
	}
}

func TestProductionPoliciesContainOnlyFixedReadQueries(t *testing.T) {
	tests := []struct {
		name   string
		policy authhttp.Policy
		want   authhttp.Policy
	}{
		{name: "kodo", policy: kodoPolicy(), want: authhttp.Policy{Host: "api.qiniuapi.com", Endpoints: []authhttp.Endpoint{
			{Method: http.MethodGet, Path: "/v6/space", Name: "storage_standard"},
			{Method: http.MethodGet, Path: "/v6/count", Name: "objects_standard"},
			{Method: http.MethodGet, Path: "/v6/space_line", Name: "storage_ia"},
			{Method: http.MethodGet, Path: "/v6/count_line", Name: "objects_ia"},
			{Method: http.MethodGet, Path: "/v6/space_intelligent_tiering", Name: "storage_intelligent_tiering"},
			{Method: http.MethodGet, Path: "/v6/count_intelligent_tiering", Name: "objects_intelligent_tiering"},
			{Method: http.MethodGet, Path: "/v6/space_archive_ir", Name: "storage_archive_ir"},
			{Method: http.MethodGet, Path: "/v6/count_archive_ir", Name: "objects_archive_ir"},
			{Method: http.MethodGet, Path: "/v6/space_archive", Name: "storage_archive"},
			{Method: http.MethodGet, Path: "/v6/count_archive", Name: "objects_archive"},
			{Method: http.MethodGet, Path: "/v6/space_deep_archive", Name: "storage_deep_archive"},
			{Method: http.MethodGet, Path: "/v6/count_deep_archive", Name: "objects_deep_archive"},
			{Method: http.MethodGet, Path: "/v6/blob_io", Name: "blob_io"},
			{Method: http.MethodGet, Path: "/v6/rs_put", Name: "rs_put"},
		}}},
		{name: "kodo discovery", policy: kodoDiscoveryPolicy(), want: authhttp.Policy{Host: "uc.qiniuapi.com", Endpoints: []authhttp.Endpoint{
			{Method: http.MethodGet, Path: "/buckets", Name: "list_buckets"},
		}}},
		{name: "cdn", policy: cdnPolicy(), want: authhttp.Policy{Host: "fusion.qiniuapi.com", Endpoints: []authhttp.Endpoint{
			{Method: http.MethodPost, Path: "/v2/tune/bandwidth", Name: "metering_bandwidth"},
			{Method: http.MethodPost, Path: "/v2/tune/flux", Name: "metering_flux"},
			{Method: http.MethodPost, Path: "/v2/tune/monitoring/bandwidth", Name: "monitoring_bandwidth"},
			{Method: http.MethodPost, Path: "/v2/tune/monitoring/flow", Name: "monitoring_flow"},
			{Method: http.MethodPost, Path: "/v2/tune/loganalyze/reqcount", Name: "request_count"},
			{Method: http.MethodPost, Path: "/v2/tune/loganalyze/statuscode", Name: "status_code"},
			{Method: http.MethodPost, Path: "/v2/tune/loganalyze/hitmiss", Name: "hit_miss"},
		}}},
		{name: "cdn discovery", policy: cdnDiscoveryPolicy(), want: authhttp.Policy{Host: "api.qiniu.com", Endpoints: []authhttp.Endpoint{
			{Method: http.MethodGet, Path: "/domain", Name: "list_domains"},
		}}},
		{name: "billing", policy: billingPolicy(), want: authhttp.Policy{Host: "api.qiniu.com", Endpoints: []authhttp.Endpoint{
			{Method: http.MethodGet, Path: "/billing-api/v1/account/balance-overview", Name: "balance_overview"},
			{Method: http.MethodGet, Path: "/billing-api/v2/bill/snapshot", Name: "bill_snapshot"},
			{Method: http.MethodGet, Path: "/billing-api/v1/respack/month-overview", Name: "resource_pack_overview"},
			{Method: http.MethodGet, Path: "/billing-api/v2/bill/detail", Name: "bill_detail"},
		}}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if !reflect.DeepEqual(test.policy, test.want) {
				t.Fatalf("production endpoint policy changed\n got: %#v\nwant: %#v", test.policy, test.want)
			}
		})
	}
}
