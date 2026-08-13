package cdn

import (
	"errors"
	"testing"
)

func TestMergeTopIPResponsesSumsBatchesAndRetainsTopTen(t *testing.T) {
	traffic := []TopIPResponse{
		{Data: TopIPData{IPs: []string{"192.0.2.1", "192.0.2.2"}, Traffic: []float64{100, 40}}},
		{Data: TopIPData{IPs: []string{"192.0.2.2", "2001:db8::1"}, Traffic: []float64{80, 50}}},
	}
	requests := []TopIPResponse{
		{Data: TopIPData{IPs: []string{"192.0.2.1"}, Count: []float64{3}}},
		{Data: TopIPData{IPs: []string{"192.0.2.1", "2001:db8::1"}, Count: []float64{4, 9}}},
	}

	got, err := MergeTopIPResponses(traffic, requests)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Traffic) != 3 || got.Traffic[0] != (TopIPValue{IP: "192.0.2.2", Value: 120}) || got.Traffic[1].IP != "192.0.2.1" {
		t.Fatalf("traffic=%#v", got.Traffic)
	}
	if len(got.Requests) != 2 || got.Requests[0] != (TopIPValue{IP: "2001:db8::1", Value: 9}) || got.Requests[1].Value != 7 {
		t.Fatalf("requests=%#v", got.Requests)
	}
}

func TestMergeTopIPResponsesCanonicalizesEquivalentIPv6Addresses(t *testing.T) {
	got, err := MergeTopIPResponses([]TopIPResponse{
		{Data: TopIPData{IPs: []string{"2001:db8:0:0:0:0:0:1"}, Traffic: []float64{2}}},
		{Data: TopIPData{IPs: []string{"2001:db8::1"}, Traffic: []float64{3}}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Traffic) != 1 || got.Traffic[0] != (TopIPValue{IP: "2001:db8::1", Value: 5}) {
		t.Fatalf("traffic=%#v", got.Traffic)
	}
}

func TestMergeTopIPResponsesRejectsInvalidData(t *testing.T) {
	_, err := MergeTopIPResponses([]TopIPResponse{{Data: TopIPData{
		IPs: []string{"not-an-ip"}, Traffic: []float64{1},
	}}}, nil)
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("error=%v, want ErrUnexpectedResponse", err)
	}

	_, err = MergeTopIPResponses([]TopIPResponse{{Data: TopIPData{
		IPs: []string{"192.0.2.1"}, Traffic: nil,
	}}}, nil)
	if !errors.Is(err, ErrSeriesMisaligned) {
		t.Fatalf("error=%v, want ErrSeriesMisaligned", err)
	}

	ips := make([]string, maxTopIPResponseItems+1)
	values := make([]float64, len(ips))
	for index := range ips {
		ips[index] = "192.0.2.1"
	}
	_, err = MergeTopIPResponses([]TopIPResponse{{Data: TopIPData{IPs: ips, Traffic: values}}}, nil)
	if !errors.Is(err, ErrUnexpectedResponse) {
		t.Fatalf("oversized response error=%v, want ErrUnexpectedResponse", err)
	}
}
