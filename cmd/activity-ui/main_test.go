package main

import (
	"encoding/json"
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
