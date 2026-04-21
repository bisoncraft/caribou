package msgjson

import (
	"errors"
	"strings"
	"testing"
)

func TestMessageUnmarshal(t *testing.T) {
	// Test null payload handling. NO error.
	msg := Message{
		Payload: []byte(`null`),
	}

	type Payload struct{}

	payload := new(Payload)
	err := msg.Unmarshal(payload) // *Payload
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if payload == nil {
		t.Errorf("expected payload to not be nil")
	}

	payload = new(Payload)
	err = msg.Unmarshal(&payload) // **Payload
	if err != nil {
		t.Errorf("expected no error, got %v", err)
	}
	if payload != nil {
		t.Errorf("expected payload to be nil")
	}
}

func TestMessageResponse(t *testing.T) {
	// Test null payload, expecting errNullRespPayload.
	msg := Message{
		Type:    Response,
		Payload: []byte(`null`),
	}

	respPayload, err := msg.Response()
	if !errors.Is(err, errNullRespPayload) {
		t.Fatalf("expected a errNullRespPayload, got %v", err)
	}
	if respPayload != nil {
		t.Errorf("response payload should have been nil")
	}

	// Test bad/empty json, expecting "unexpected end of JSON input".
	msg.Payload = []byte(``)
	respPayload, err = msg.Response()
	const wantErrStr = "unexpected end of JSON input"
	if err == nil || !strings.Contains(err.Error(), wantErrStr) {
		t.Fatalf("expected error with %q, got %q", wantErrStr, err)
	}
	if respPayload != nil {
		t.Errorf("response payload should have been nil")
	}
}

func TestNewRequestResponse(t *testing.T) {
	// id=0 must be rejected.
	_, err := NewRequest(0, "route", nil)
	if err == nil {
		t.Error("expected error for id=0 request")
	}
	// empty route must be rejected.
	_, err = NewRequest(1, "", nil)
	if err == nil {
		t.Error("expected error for empty route request")
	}
	// valid request.
	msg, err := NewRequest(1, "test", map[string]string{"k": "v"})
	if err != nil {
		t.Fatalf("NewRequest error: %v", err)
	}
	if msg.Type != Request {
		t.Errorf("expected Request type, got %v", msg.Type)
	}

	// NewResponse id=0 must be rejected.
	_, err = NewResponse(0, nil, nil)
	if err == nil {
		t.Error("expected error for id=0 response")
	}
	// valid response.
	resp, err := NewResponse(1, "result", nil)
	if err != nil {
		t.Fatalf("NewResponse error: %v", err)
	}
	if resp.Type != Response {
		t.Errorf("expected Response type, got %v", resp.Type)
	}
}

func TestNewNotification(t *testing.T) {
	// empty route must be rejected.
	_, err := NewNotification("", nil)
	if err == nil {
		t.Error("expected error for empty route notification")
	}
	msg, err := NewNotification("test", map[string]int{"n": 1})
	if err != nil {
		t.Fatalf("NewNotification error: %v", err)
	}
	if msg.Type != Notification {
		t.Errorf("expected Notification type, got %v", msg.Type)
	}
}

func TestErrorCodes(t *testing.T) {
	if RPCParseError != 1 {
		t.Errorf("RPCParseError expected 1, got %d", RPCParseError)
	}
	if RPCInternalError != 7 {
		t.Errorf("RPCInternalError expected 7, got %d", RPCInternalError)
	}
	if UnknownMessageType != 46 {
		t.Errorf("UnknownMessageType expected 46, got %d", UnknownMessageType)
	}
}
