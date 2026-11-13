package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// rpcExecutorRequest mirrors the host wire schema for executor calls: the
// embedded pluginapi.ExecutorRequest plus the async stream plumbing fields.
type rpcExecutorRequest struct {
	pluginapi.ExecutorRequest
	StreamID       string `json:"stream_id,omitempty"`
	HostCallbackID string `json:"host_callback_id,omitempty"`
}

// rpcExecutorStreamResponse mirrors the host wire schema for executor
// streaming responses. Returning headers without chunks switches the host to
// async streaming mode, where chunks arrive via host.stream.emit callbacks.
type rpcExecutorStreamResponse struct {
	Headers http.Header                     `json:"headers,omitempty"`
	Chunks  []pluginapi.ExecutorStreamChunk `json:"chunks,omitempty"`
}

const (
	qoderChatPath     = "/algo/api/v2/service/pro/sse/agent_chat_generation?FetchKeys=llm_model_result&AgentId=agent_common&Encode=1"
	maxErrorBodyLen   = 512
	headerAccept      = "Accept"
	headerAuth        = "Authorization"
	headerCache       = "Cache-Control"
	headerContent     = "Content-Type"
	headerEncoding    = "Accept-Encoding"
	headerModelKey    = "X-Model-Key"
	headerModelSource = "X-Model-Source"
)

// buildQoderChatRequest converts an OpenAI request to Qoder's native payload,
// encodes the wire body, and signs exactly those bytes.
func buildQoderChatRequest(req pluginapi.ExecutorRequest, cfg pluginConfig, creds qoderCredentials) (hostHTTPRequest, string, error) {
	model := mapModel(cfg, req.Model)
	payload, errPayload := buildQoderRequestPayload(req.Payload, model, creds.UserID)
	if errPayload != nil {
		return hostHTTPRequest{}, "", errPayload
	}
	wireBody := encodeQoderBody(payload)
	requestURL := cfg.BaseURL + qoderChatPath
	headers, errHeaders := buildCosyHeaders(wireBody, requestURL, creds)
	if errHeaders != nil {
		return hostHTTPRequest{}, "", fmt.Errorf("build qoder request authentication: %w", errHeaders)
	}
	headers[headerContent] = []string{"application/json"}
	headers[headerAccept] = []string{"text/event-stream"}
	headers[headerCache] = []string{"no-cache"}
	headers[headerEncoding] = []string{"identity"}
	headers[headerModelKey] = []string{model}
	headers[headerModelSource] = []string{"system"}
	return hostHTTPRequest{
		Method:  http.MethodPost,
		URL:     requestURL,
		Headers: headers,
		Body:    wireBody,
	}, model, nil
}

// handleExecutorExecute implements executor.execute for non-streaming clients.
// Qoder always responds with SSE, which is buffered and aggregated here.
func handleExecutorExecute(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	creds, errCredentials := resolveQoderCredentials(req.StorageJSON, cfg)
	if errCredentials != nil {
		return errorEnvelope("invalid_auth", errCredentials.Error()), nil
	}
	upstreamRequest, model, errRequest := buildQoderChatRequest(req.ExecutorRequest, cfg, creds)
	if errRequest != nil {
		return errorEnvelope("invalid_request", errRequest.Error()), nil
	}
	response, errDo := hostHTTPDo(upstreamRequest)
	if errDo != nil {
		return errorEnvelope("upstream_unavailable", errDo.Error()), nil
	}
	if response.StatusCode >= http.StatusBadRequest {
		return errorEnvelope("upstream_error", upstreamErrorMessage(response.StatusCode, response.Body, creds.JobToken)), nil
	}
	body, errConvert := convertQoderSSEResponse(response.Body, model)
	if errConvert != nil {
		return errorEnvelope("invalid_response", errConvert.Error()), nil
	}
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: body,
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// handleExecutorExecuteStream opens Qoder's SSE stream synchronously so
// authentication and quota failures surface before async forwarding starts.
func handleExecutorExecuteStream(raw []byte) ([]byte, error) {
	var req rpcExecutorRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	if req.StreamID == "" {
		return errorEnvelope("invalid_request", "executor stream id is missing"), nil
	}
	cfg := loadedConfig()
	creds, errCredentials := resolveQoderCredentials(req.StorageJSON, cfg)
	if errCredentials != nil {
		return errorEnvelope("invalid_auth", errCredentials.Error()), nil
	}
	upstreamRequest, model, errRequest := buildQoderChatRequest(req.ExecutorRequest, cfg, creds)
	if errRequest != nil {
		return errorEnvelope("invalid_request", errRequest.Error()), nil
	}
	upstream, errDo := hostHTTPDoStream(upstreamRequest)
	if errDo != nil {
		return errorEnvelope("upstream_unavailable", errDo.Error()), nil
	}
	if upstream.StatusCode >= http.StatusBadRequest {
		body := drainHostHTTPStream(upstream.StreamID)
		return errorEnvelope("upstream_error", upstreamErrorMessage(upstream.StatusCode, body, creds.JobToken)), nil
	}
	go pumpQoderStream(upstream.StreamID, req.StreamID, model)
	return okEnvelope(rpcExecutorStreamResponse{
		Headers: http.Header{"Content-Type": []string{"text/event-stream"}},
	})
}

// pumpQoderStream reads upstream SSE byte chunks, converts them into OpenAI
// SSE frames, and forwards them to the downstream client stream. It stops when
// the upstream stream ends or the host closes the client stream.
func pumpQoderStream(upstreamStreamID, clientStreamID, model string) {
	converter := newQoderStreamConverter(model)
	splitter := &sseSplitter{}
	defer hostHTTPStreamClose(upstreamStreamID)
	defer hostStreamClose(clientStreamID, "")

	emit := func(frames [][]byte) bool {
		for _, frame := range frames {
			if len(frame) == 0 {
				continue
			}
			if errEmit := hostStreamEmit(clientStreamID, frame); errEmit != nil {
				return false
			}
		}
		return true
	}

	for {
		chunk, errRead := hostHTTPStreamRead(upstreamStreamID)
		if errRead != nil {
			_ = emit([][]byte{makeErrorFrame("qoder upstream read failed: " + errRead.Error())})
			_ = emit(converter.finish())
			return
		}
		if chunk.Error != "" {
			_ = emit([][]byte{makeErrorFrame("qoder upstream stream failed: " + chunk.Error)})
			_ = emit(converter.finish())
			return
		}
		if len(chunk.Payload) > 0 {
			for _, line := range splitter.feed(chunk.Payload) {
				frames, errConvert := converter.convertLine([]byte(line))
				if errConvert != nil {
					_ = emit([][]byte{makeErrorFrame("invalid qoder stream response: " + errConvert.Error())})
					_ = emit(converter.finish())
					return
				}
				if !emit(frames) {
					return
				}
			}
		}
		if chunk.Done {
			for _, line := range splitter.feed([]byte("\n")) {
				frames, errConvert := converter.convertLine([]byte(line))
				if errConvert != nil {
					_ = emit([][]byte{makeErrorFrame("invalid qoder stream response: " + errConvert.Error())})
					break
				}
				if !emit(frames) {
					return
				}
			}
			_ = emit(converter.finish())
			return
		}
	}
}

// drainHostHTTPStream consumes the remaining upstream stream (used to collect
// an error body) and always closes the host-side stream handle.
func drainHostHTTPStream(streamID string) []byte {
	if streamID == "" {
		return nil
	}
	defer hostHTTPStreamClose(streamID)
	var body strings.Builder
	for {
		chunk, errRead := hostHTTPStreamRead(streamID)
		if errRead != nil {
			break
		}
		if len(chunk.Payload) > 0 && body.Len() < maxErrorBodyLen {
			remaining := maxErrorBodyLen - body.Len()
			payload := chunk.Payload
			if len(payload) > remaining {
				payload = payload[:remaining]
			}
			body.Write(payload)
		}
		if chunk.Done {
			break
		}
	}
	return []byte(body.String())
}

// handleExecutorCountTokens implements executor.count_tokens. Qoder does not
// expose a token counting endpoint, so a zero estimate is returned.
func handleExecutorCountTokens(_ []byte) ([]byte, error) {
	return okEnvelope(pluginapi.ExecutorResponse{
		Payload: []byte(`{"input_tokens":0,"total_tokens":0}`),
		Headers: http.Header{"Content-Type": []string{"application/json"}},
	})
}

// handleExecutorHTTPRequest signs provider-native requests with ephemeral Qoder
// credentials. Client authorization and COSY headers are always discarded.
func handleExecutorHTTPRequest(raw []byte) ([]byte, error) {
	var req pluginapi.ExecutorHTTPRequest
	if errUnmarshal := json.Unmarshal(raw, &req); errUnmarshal != nil {
		return nil, errUnmarshal
	}
	cfg := loadedConfig()
	if !allowedQoderRequestURL(req.URL, cfg) {
		return errorEnvelope("invalid_request", "request URL is not an allowed Qoder endpoint"), nil
	}
	creds, errCredentials := resolveQoderCredentials(req.StorageJSON, cfg)
	if errCredentials != nil {
		return errorEnvelope("invalid_auth", errCredentials.Error()), nil
	}
	headers := httpHeaders{}
	for key, values := range req.Headers {
		if strings.EqualFold(key, headerAuth) ||
			strings.EqualFold(key, "Host") ||
			strings.EqualFold(key, "Content-Length") ||
			strings.HasPrefix(strings.ToLower(key), "cosy-") {
			continue
		}
		headers[key] = append([]string(nil), values...)
	}
	cosyHeaders, errHeaders := buildCosyHeaders(req.Body, req.URL, creds)
	if errHeaders != nil {
		return errorEnvelope("invalid_request", "could not authenticate Qoder request"), nil
	}
	for key, values := range cosyHeaders {
		headers[key] = values
	}
	resp, errDo := hostHTTPDo(hostHTTPRequest{
		Method:  req.Method,
		URL:     req.URL,
		Headers: headers,
		Body:    req.Body,
	})
	if errDo != nil {
		return errorEnvelope("upstream_unavailable", errDo.Error()), nil
	}
	return okEnvelope(pluginapi.ExecutorHTTPResponse{
		StatusCode: resp.StatusCode,
		Headers:    resp.Headers,
		Body:       resp.Body,
	})
}

func allowedQoderRequestURL(rawURL string, cfg pluginConfig) bool {
	requestURL, errRequestURL := url.Parse(rawURL)
	if errRequestURL != nil || requestURL.Scheme == "" || requestURL.Host == "" || requestURL.User != nil {
		return false
	}
	for _, configuredBase := range []string{cfg.BaseURL, cfg.OpenAPIBaseURL} {
		baseURL, errBaseURL := url.Parse(configuredBase)
		if errBaseURL == nil &&
			strings.EqualFold(requestURL.Scheme, baseURL.Scheme) &&
			strings.EqualFold(requestURL.Host, baseURL.Host) {
			return true
		}
	}
	return false
}

// upstreamErrorMessage builds a compact error message from an upstream HTTP
// failure without leaking credentials.
func upstreamErrorMessage(statusCode int, body []byte, secrets ...string) string {
	snippet := strings.TrimSpace(string(body))
	for _, secret := range secrets {
		if secret != "" {
			snippet = strings.ReplaceAll(snippet, secret, "[REDACTED]")
		}
	}
	if len(snippet) > maxErrorBodyLen {
		snippet = snippet[:maxErrorBodyLen] + "..."
	}
	if snippet == "" {
		return fmt.Sprintf("qoder upstream returned status %d", statusCode)
	}
	return fmt.Sprintf("qoder upstream returned status %d: %s", statusCode, snippet)
}

// makeErrorFrame builds an OpenAI style stream error payload.
func makeErrorFrame(message string) []byte {
	payload := map[string]any{
		"error": map[string]string{
			"message": message,
			"type":    "upstream_error",
		},
	}
	raw, errMarshal := json.Marshal(payload)
	if errMarshal != nil {
		return nil
	}
	return raw
}
