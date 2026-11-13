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
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		userID = r.Header.Get("X-User-Id")
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
}

func TestHandleIndexDisablesBrowserCaching(t *testing.T) {
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	response := httptest.NewRecorder()

	(&app{}).handleIndex(response, request)

	if cacheControl := response.Header().Get("Cache-Control"); cacheControl != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cacheControl)
	}
}
