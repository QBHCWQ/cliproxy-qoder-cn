package main

import (
	"encoding/json"
	"fmt"
	"net/http"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

type qoderQuotaBalance struct {
	Total     float64 `json:"total"`
	Used      float64 `json:"used"`
	Remaining float64 `json:"remaining"`
}

func handleQuotaFetch(raw []byte) ([]byte, error) {
	var req pluginapi.QuotaFetchRequest
	if err := json.Unmarshal(raw, &req); err != nil {
		return nil, err
	}
	storage := req.StorageJSON
	if len(storage) == 0 {
		var err error
		storage, err = json.Marshal(req.Metadata)
		if err != nil {
			return nil, err
		}
	}
	cfg := loadedConfig()
	creds, err := resolveQoderCredentials(storage, cfg)
	if err != nil {
		return errorEnvelope("invalid_auth", "Could not authenticate Qoder quota query"), nil
	}
	resp, err := hostHTTPDo(hostHTTPRequest{
		Method: "GET", URL: cfg.OpenAPIBaseURL + "/api/v2/quota/usage",
		Headers: httpHeaders{"Authorization": {"Bearer " + creds.JobToken}, "Accept": {"application/json"}},
	})
	if err != nil {
		return errorEnvelope("upstream_unavailable", "Qoder quota query failed"), nil
	}
	if resp.StatusCode != http.StatusOK {
		return errorEnvelope("upstream_error", fmt.Sprintf("Qoder quota returned HTTP %d", resp.StatusCode)), nil
	}
	var usage struct {
		UserType string             `json:"userType"`
		Plan     *qoderQuotaBalance `json:"userQuota"`
		AddOn    *qoderQuotaBalance `json:"addOnQuota"`
	}
	if err := json.Unmarshal(resp.Body, &usage); err != nil {
		return errorEnvelope("invalid_response", "Invalid Qoder quota response"), nil
	}
	out := pluginapi.QuotaFetchResponse{Subscription: &pluginapi.QuotaSubscription{Plan: usage.UserType}}
	for _, item := range []struct {
		key, label string
		balance    *qoderQuotaBalance
	}{{"plan", "套餐", usage.Plan}, {"addon", "加油包", usage.AddOn}} {
		if item.balance == nil {
			continue
		}
		for _, metric := range []struct {
			key, label string
			value      float64
		}{{"total", "总额", item.balance.Total}, {"used", "已用", item.balance.Used}, {"remaining", "剩余", item.balance.Remaining}} {
			out.Summary = append(out.Summary, pluginapi.QuotaMetric{Key: item.key + "_" + metric.key, Label: item.label + metric.label, Value: metric.value, Unit: "Credits"})
		}
	}
	if len(out.Summary) == 0 {
		return errorEnvelope("invalid_response", "Qoder returned no credit balances"), nil
	}
	return okEnvelope(out)
}
