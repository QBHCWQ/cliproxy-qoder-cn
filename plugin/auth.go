package main

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// qoderAuthFile is the auth JSON layout the plugin accepts in the CPA auth
// directory (for example auths/qoder-work.json).
//
//	{
//	  "type": "qoder",
//	  "personal_access_token": "qod-...",
//	  "label": "Qoder Work",                      // optional
//	  "base_url": "https://openapi.qoder.sh",     // optional override
//	  "prefix": ""                                // optional model prefix
//	}
type qoderAuthFile struct {
	Type                string `json:"type"`
	PersonalAccessToken string `json:"personal_access_token"`
	Label               string `json:"label"`
	BaseURL             string `json:"base_url"`
	Prefix              string `json:"prefix"`
}

type qoderCredentials struct {
	JobToken  string
	UserID    string
	Name      string
	Email     string
	MachineID string
	ExpiresAt time.Time
}

const qoderCredentialRefreshBuffer = 5 * time.Minute

type qoderCredentialCacheEntry struct {
	Credentials qoderCredentials
}

func (entry qoderCredentialCacheEntry) freshAt(now time.Time) bool {
	return entry.Credentials.JobToken != "" &&
		entry.Credentials.ExpiresAt.After(now.Add(qoderCredentialRefreshBuffer))
}

type qoderCredentialExchange struct {
	Done        chan struct{}
	Credentials qoderCredentials
	Err         error
}

var qoderCredentialCache = struct {
	sync.Mutex
	Entries  map[[sha256.Size]byte]qoderCredentialCacheEntry
	InFlight map[[sha256.Size]byte]*qoderCredentialExchange
}{
	Entries:  make(map[[sha256.Size]byte]qoderCredentialCacheEntry),
	InFlight: make(map[[sha256.Size]byte]*qoderCredentialExchange),
}

// parseAuthFile validates and decodes raw auth file content. It returns nil
// when the content is not a Qoder auth file.
func parseAuthFile(raw []byte) *qoderAuthFile {
	var file qoderAuthFile
	if errUnmarshal := json.Unmarshal(raw, &file); errUnmarshal != nil {
		return nil
	}
	if !strings.EqualFold(strings.TrimSpace(file.Type), pluginIdentifier) {
		return nil
	}
	if strings.TrimSpace(file.PersonalAccessToken) == "" {
		return nil
	}
	return &file
}

func resolveQoderCredentials(storageJSON []byte, cfg pluginConfig) (qoderCredentials, error) {
	file := parseAuthFile(storageJSON)
	if file == nil {
		return qoderCredentials{}, fmt.Errorf("qoder auth storage is missing a personal access token")
	}
	pat := strings.TrimSpace(file.PersonalAccessToken)
	cacheKey := sha256.Sum256([]byte(pat))
	now := time.Now()

	qoderCredentialCache.Lock()
	if cached, ok := qoderCredentialCache.Entries[cacheKey]; ok && cached.freshAt(now) {
		qoderCredentialCache.Unlock()
		return cached.Credentials, nil
	}
	if exchange, ok := qoderCredentialCache.InFlight[cacheKey]; ok {
		qoderCredentialCache.Unlock()
		<-exchange.Done
		return exchange.Credentials, exchange.Err
	}
	exchange := &qoderCredentialExchange{Done: make(chan struct{})}
	qoderCredentialCache.InFlight[cacheKey] = exchange
	qoderCredentialCache.Unlock()

	credentials, errResolve := exchangeQoderCredentials(pat, cfg, now)

	qoderCredentialCache.Lock()
	if errResolve == nil {
		qoderCredentialCache.Entries[cacheKey] = qoderCredentialCacheEntry{Credentials: credentials}
	}
	exchange.Credentials = credentials
	exchange.Err = errResolve
	delete(qoderCredentialCache.InFlight, cacheKey)
	close(exchange.Done)
	qoderCredentialCache.Unlock()
	return credentials, errResolve
}

type qoderJobTokenResponse struct {
	Token     string          `json:"token"`
	ExpiresAt json.RawMessage `json:"expires_at"`
	ExpiresIn int64           `json:"expires_in"`
}

type qoderUserInfoResponse struct {
	ID       json.RawMessage `json:"id"`
	Name     string          `json:"name"`
	Username string          `json:"username"`
	Email    string          `json:"email"`
}

func exchangeQoderCredentials(pat string, cfg pluginConfig, now time.Time) (qoderCredentials, error) {
	openAPIBaseURL := strings.TrimRight(strings.TrimSpace(cfg.OpenAPIBaseURL), "/")
	if openAPIBaseURL == "" {
		openAPIBaseURL = defaultOpenAPIBaseURL
	}
	requestBody, errMarshal := json.Marshal(map[string]string{"personal_token": pat})
	if errMarshal != nil {
		return qoderCredentials{}, fmt.Errorf("qoder PAT exchange request: %w", errMarshal)
	}
	exchangeResponse, errExchange := hostHTTPDo(hostHTTPRequest{
		Method: "POST",
		URL:    openAPIBaseURL + "/api/v1/jobToken/exchange",
		Headers: httpHeaders{
			"Accept":          {"application/json"},
			"Content-Type":    {"application/json"},
			"Cosy-Version":    {cosyVersion},
			"Cosy-ClientType": {cosyClientType},
			"User-Agent":      {"CLIProxyAPI-qoder"},
		},
		Body: requestBody,
	})
	if errExchange != nil {
		return qoderCredentials{}, fmt.Errorf("qoder PAT exchange request failed")
	}
	if exchangeResponse.StatusCode < 200 || exchangeResponse.StatusCode >= 300 {
		return qoderCredentials{}, fmt.Errorf("qoder PAT exchange failed with HTTP status %d", exchangeResponse.StatusCode)
	}
	var tokenResponse qoderJobTokenResponse
	if errDecode := json.Unmarshal(exchangeResponse.Body, &tokenResponse); errDecode != nil {
		return qoderCredentials{}, fmt.Errorf("decode qoder PAT exchange response: %w", errDecode)
	}
	jobToken := strings.TrimSpace(tokenResponse.Token)
	if jobToken == "" {
		return qoderCredentials{}, fmt.Errorf("qoder PAT exchange response is missing a job token")
	}
	expiresAt := qoderJobTokenExpiry(tokenResponse, now)

	userInfoResponse, errUserInfo := hostHTTPDo(hostHTTPRequest{
		Method: "GET",
		URL:    openAPIBaseURL + "/api/v1/userinfo",
		Headers: httpHeaders{
			"Accept":          {"application/json"},
			"Authorization":   {"Bearer " + jobToken},
			"Cosy-Version":    {cosyVersion},
			"Cosy-ClientType": {cosyClientType},
			"User-Agent":      {"CLIProxyAPI-qoder"},
		},
	})
	if errUserInfo != nil {
		return qoderCredentials{}, fmt.Errorf("qoder userinfo request failed")
	}
	if userInfoResponse.StatusCode < 200 || userInfoResponse.StatusCode >= 300 {
		return qoderCredentials{}, fmt.Errorf("qoder userinfo failed with HTTP status %d", userInfoResponse.StatusCode)
	}
	var userInfo qoderUserInfoResponse
	if errDecode := json.Unmarshal(userInfoResponse.Body, &userInfo); errDecode != nil {
		return qoderCredentials{}, fmt.Errorf("decode qoder userinfo response: %w", errDecode)
	}
	userID := qoderJSONIdentifier(userInfo.ID)
	if userID == "" {
		return qoderCredentials{}, fmt.Errorf("qoder userinfo response is missing a user id")
	}
	name := strings.TrimSpace(userInfo.Name)
	if name == "" {
		name = strings.TrimSpace(userInfo.Username)
	}
	return qoderCredentials{
		JobToken:  jobToken,
		UserID:    userID,
		Name:      name,
		Email:     strings.TrimSpace(userInfo.Email),
		MachineID: resolveMachineID(cfg.MachineID),
		ExpiresAt: expiresAt,
	}, nil
}

func qoderJobTokenExpiry(response qoderJobTokenResponse, now time.Time) time.Time {
	if len(response.ExpiresAt) > 0 && string(response.ExpiresAt) != "null" {
		var text string
		if json.Unmarshal(response.ExpiresAt, &text) == nil {
			if parsed, errParse := time.Parse(time.RFC3339, text); errParse == nil {
				return parsed
			}
			if unix, errUnix := strconv.ParseInt(text, 10, 64); errUnix == nil {
				return qoderUnixTime(unix)
			}
		}
		var unix int64
		if json.Unmarshal(response.ExpiresAt, &unix) == nil {
			return qoderUnixTime(unix)
		}
	}
	if response.ExpiresIn > 0 {
		return now.Add(time.Duration(response.ExpiresIn) * time.Millisecond)
	}
	return now.Add(24 * time.Hour)
}

func qoderUnixTime(value int64) time.Time {
	if value > 1_000_000_000_000 {
		return time.UnixMilli(value)
	}
	return time.Unix(value, 0)
}

func qoderJSONIdentifier(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return strings.TrimSpace(text)
	}
	var number json.Number
	if json.Unmarshal(raw, &number) == nil {
		return number.String()
	}
	return ""
}

// authDataFromFile builds the host auth record for a parsed Qoder auth file.
func authDataFromFile(req pluginapi.AuthParseRequest, file *qoderAuthFile) pluginapi.AuthData {
	label := strings.TrimSpace(file.Label)
	if label == "" {
		label = strings.TrimSuffix(req.FileName, ".json")
	}
	id := strings.TrimSpace(req.FileName)
	if id == "" {
		id = label
	}
	attributes := map[string]string{}
	if baseURL := strings.TrimRight(strings.TrimSpace(file.BaseURL), "/"); baseURL != "" {
		attributes["base_url"] = baseURL
	}
	return pluginapi.AuthData{
		Provider:    pluginIdentifier,
		ID:          id,
		FileName:    req.FileName,
		Label:       label,
		Prefix:      strings.TrimSpace(file.Prefix),
		StorageJSON: append([]byte(nil), req.RawJSON...),
		Metadata:    map[string]any{"type": pluginIdentifier},
		Attributes:  attributes,
	}
}

// handleAuthParse implements the auth.parse RPC method.
func handleAuthParse(raw []byte) ([]byte, error) {
	var req pluginapi.AuthParseRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if provider := strings.TrimSpace(req.Provider); provider != "" && !strings.EqualFold(provider, pluginIdentifier) {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	file := parseAuthFile(req.RawJSON)
	if file == nil {
		return okEnvelope(pluginapi.AuthParseResponse{Handled: false})
	}
	return okEnvelope(pluginapi.AuthParseResponse{
		Handled: true,
		Auth:    authDataFromFile(req, file),
	})
}

// handleAuthLoginStart implements auth.login.start. Qoder uses personal access
// tokens created on the account integrations page, so there is no interactive
// login flow; the message guides the user instead.
func handleAuthLoginStart(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.AuthLoginStartResponse{
		Provider:  pluginIdentifier,
		URL:       "https://qoder.com/account/integrations",
		State:     "manual-pat",
		ExpiresAt: time.Now().Add(time.Minute).UTC(),
	})
}

// handleAuthLoginPoll implements auth.login.poll.
func handleAuthLoginPoll(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.AuthLoginPollResponse{
		Status:  pluginapi.AuthLoginStatusError,
		Message: "Qoder login is token based: create a personal access token at https://qoder.com/account/integrations and save it as a qoder auth JSON file (see plugin docs).",
	})
}

// handleAuthRefresh validates the PAT by prewarming its short-lived job token.
func handleAuthRefresh(raw []byte) ([]byte, error) {
	var req pluginapi.AuthRefreshRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	file := parseAuthFile(req.StorageJSON)
	if file == nil {
		return errorEnvelope("invalid_auth", "qoder auth storage is missing a personal access token"), nil
	}
	credentials, errResolve := resolveQoderCredentials(req.StorageJSON, loadedConfig())
	if errResolve != nil {
		return errorEnvelope("refresh_failed", errResolve.Error()), nil
	}
	label := strings.TrimSpace(file.Label)
	if label == "" {
		label = req.AuthID
	}
	attributes := map[string]string{}
	if baseURL := strings.TrimRight(strings.TrimSpace(file.BaseURL), "/"); baseURL != "" {
		attributes["base_url"] = baseURL
	}
	nextRefresh := credentials.ExpiresAt.Add(-qoderCredentialRefreshBuffer)
	if !nextRefresh.After(time.Now()) {
		nextRefresh = time.Now().Add(time.Minute)
	}
	return okEnvelope(pluginapi.AuthRefreshResponse{
		Auth: pluginapi.AuthData{
			Provider:    pluginIdentifier,
			ID:          req.AuthID,
			FileName:    req.AuthID,
			Label:       label,
			Prefix:      strings.TrimSpace(file.Prefix),
			StorageJSON: append([]byte(nil), req.StorageJSON...),
			Metadata:    map[string]any{"type": pluginIdentifier},
			Attributes:  attributes,
		},
		NextRefreshAfter: nextRefresh.UTC(),
	})
}
