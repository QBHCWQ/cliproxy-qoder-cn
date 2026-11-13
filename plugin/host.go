package main

import (
	"encoding/json"
	"fmt"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginabi"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/pluginapi"
)

// This file wraps the host callback RPC surface used by the plugin. All calls
// go through callHost (defined in main.go), which bridges into the host
// process via the C ABI function table handed over at plugin init.

// hostHTTPRequest is the wire schema for host.http.do and host.http.do_stream.
type hostHTTPRequest struct {
	Method  string      `json:"method,omitempty"`
	URL     string      `json:"url,omitempty"`
	Headers httpHeaders `json:"headers,omitempty"`
	Body    []byte      `json:"body,omitempty"`
}

type httpHeaders map[string][]string

// hostHTTPStreamResponse is the wire schema returned by host.http.do_stream.
type hostHTTPStreamResponse struct {
	StatusCode int         `json:"status_code"`
	Headers    httpHeaders `json:"headers,omitempty"`
	StreamID   string      `json:"stream_id,omitempty"`
}

// hostHTTPStreamReadResponse is the wire schema returned by host.http.stream_read.
type hostHTTPStreamReadResponse struct {
	Payload []byte `json:"payload,omitempty"`
	Error   string `json:"error,omitempty"`
	Done    bool   `json:"done,omitempty"`
}

// hostHTTPDo performs a non-streaming upstream HTTP request through the host
// transport so proxy settings and request logging apply.
func hostHTTPDo(req hostHTTPRequest) (pluginapi.HTTPResponse, error) {
	raw, errCall := callHost(pluginabi.MethodHostHTTPDo, req)
	if errCall != nil {
		return pluginapi.HTTPResponse{}, errCall
	}
	var resp pluginapi.HTTPResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return pluginapi.HTTPResponse{}, fmt.Errorf("decode host.http.do response: %w", errUnmarshal)
	}
	return resp, nil
}

// hostHTTPDoStream starts a streaming upstream HTTP request through the host
// transport and returns the host-side stream handle.
func hostHTTPDoStream(req hostHTTPRequest) (hostHTTPStreamResponse, error) {
	raw, errCall := callHost(pluginabi.MethodHostHTTPDoStream, req)
	if errCall != nil {
		return hostHTTPStreamResponse{}, errCall
	}
	var resp hostHTTPStreamResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return hostHTTPStreamResponse{}, fmt.Errorf("decode host.http.do_stream response: %w", errUnmarshal)
	}
	return resp, nil
}

// hostHTTPStreamRead reads the next chunk of a host HTTP stream.
func hostHTTPStreamRead(streamID string) (hostHTTPStreamReadResponse, error) {
	raw, errCall := callHost(pluginabi.MethodHostHTTPStreamRead, map[string]string{"stream_id": streamID})
	if errCall != nil {
		return hostHTTPStreamReadResponse{}, errCall
	}
	var resp hostHTTPStreamReadResponse
	if errUnmarshal := json.Unmarshal(raw, &resp); errUnmarshal != nil {
		return hostHTTPStreamReadResponse{}, fmt.Errorf("decode host.http.stream_read response: %w", errUnmarshal)
	}
	return resp, nil
}

// hostHTTPStreamClose releases a host HTTP stream.
func hostHTTPStreamClose(streamID string) {
	_, _ = callHost(pluginabi.MethodHostHTTPStreamClose, map[string]string{"stream_id": streamID})
}

// hostStreamEmit forwards one converted chunk to the downstream client stream.
// It returns an error once the host has closed the stream (client disconnect),
// which the pump uses as its stop signal.
func hostStreamEmit(streamID string, payload []byte) error {
	_, errCall := callHost(pluginabi.MethodHostStreamEmit, map[string]any{
		"stream_id": streamID,
		"payload":   payload,
	})
	return errCall
}

// hostStreamClose terminates the downstream client stream.
func hostStreamClose(streamID string, errMessage string) {
	_, _ = callHost(pluginabi.MethodHostStreamClose, map[string]string{
		"stream_id": streamID,
		"error":     errMessage,
	})
}

// hostLog forwards a log message to the host logger.
func hostLog(level, message string, fields map[string]any) {
	_, _ = callHost(pluginabi.MethodHostLog, map[string]any{
		"level":   level,
		"message": message,
		"fields":  fields,
	})
}
