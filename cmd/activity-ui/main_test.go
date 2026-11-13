package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestSendMessagePayloadPreservesContext(t *testing.T) {
	payload, err := sendMessagePayload(chatRequest{Message: "describe this", ContextID: "session-1"})
	if err != nil {
		t.Fatalf("sendMessagePayload: %v", err)
	}

	var decoded struct {
		Method string `json:"method"`
		Params struct {
			Message struct {
				Role      string `json:"role"`
				ContextID string `json:"contextId"`
				Parts     []struct {
					Text string `json:"text"`
				} `json:"parts"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded.Method != "SendStreamingMessage" {
		t.Fatalf("method = %q", decoded.Method)
	}
	if decoded.Params.Message.Role != "ROLE_USER" {
		t.Fatalf("role = %q", decoded.Params.Message.Role)
	}
	if decoded.Params.Message.ContextID != "session-1" {
		t.Fatalf("context = %q", decoded.Params.Message.ContextID)
	}
	if len(decoded.Params.Message.Parts) != 1 || decoded.Params.Message.Parts[0].Text != "describe this" {
		t.Fatalf("parts = %#v", decoded.Params.Message.Parts)
	}
}

func TestProxyA2AUsesConfiguredUserID(t *testing.T) {
	var userID string
	var extensions string
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID = r.Header.Get("X-User-Id")
		extensions = r.Header.Get("A2A-Extensions")
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {}\n\n"))
	}))
	defer upstream.Close()

	app := &app{httpClient: upstream.Client(), userID: "dashboard-user"}
	request := httptest.NewRequest(http.MethodPost, "/api/chat", nil)
	response := httptest.NewRecorder()

	app.proxyA2A(response, request, upstream.URL, chatRequest{Message: "investigate"})

	if userID != "dashboard-user" {
		t.Fatalf("X-User-Id = %q, want dashboard-user", userID)
	}
	if extensions != hitlExtension {
		t.Fatalf("A2A-Extensions = %q, want %q", extensions, hitlExtension)
	}
}

func TestSendMessagePayloadIncludesApprovalResponse(t *testing.T) {
	payload, err := sendMessagePayload(chatRequest{
		Message:   "approval submitted",
		ContextID: "session-1",
		TaskID:    "task-1",
		HITLResponse: map[string]any{
			"type": "tool_approval_response",
			"approvals": []map[string]any{
				{"id": "tool-1", "approved": true},
			},
		},
	})
	if err != nil {
		t.Fatalf("sendMessagePayload: %v", err)
	}

	var decoded struct {
		Params struct {
			Message struct {
				TaskID     string         `json:"taskId"`
				Extensions []string       `json:"extensions"`
				Metadata   map[string]any `json:"metadata"`
			} `json:"message"`
		} `json:"params"`
	}
	if err := json.Unmarshal(payload, &decoded); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if decoded.Params.Message.TaskID != "task-1" {
		t.Fatalf("taskId = %q", decoded.Params.Message.TaskID)
	}
	if len(decoded.Params.Message.Extensions) != 1 || decoded.Params.Message.Extensions[0] != hitlExtension {
		t.Fatalf("extensions = %#v", decoded.Params.Message.Extensions)
	}
	response, ok := decoded.Params.Message.Metadata[hitlExtension].(map[string]any)
	if !ok || response["type"] != "tool_approval_response" {
		t.Fatalf("HITL response = %#v", decoded.Params.Message.Metadata[hitlExtension])
	}
}

func TestHandleIndexDisablesBrowserCaching(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	(&app{}).handleIndex(response, request)

	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cacheControl)
	}
}
