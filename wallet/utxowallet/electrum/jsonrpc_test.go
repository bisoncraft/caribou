// Copyright (c) 2024 The Caribou Developers
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestPrepareRequest(t *testing.T) {
	tests := []struct {
		name    string
		args    any
		wantSub string // substring expected in marshalled request
		wantErr bool
	}{
		{"nil-args", nil, `"params":[]`, false},
		{"positional", positional{"foo", 42}, `"params":["foo",42]`, false},
		{"slice", []string{"a", "b"}, `"params":["a","b"]`, false},
		{"struct", struct {
			A string `json:"a"`
		}{A: "x"}, `"params":{"a":"x"}`, false},
		{"bare-int", 7, "", true},
		{"bare-string", "hi", "", true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			b, err := prepareRequest(1, "test.method", tc.args)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("expected error, got nil; payload=%s", b)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if !strings.Contains(string(b), tc.wantSub) {
				t.Fatalf("payload missing %q: %s", tc.wantSub, b)
			}
			// All requests must round-trip as JSON.
			var req rpcRequest
			if err := json.Unmarshal(b, &req); err != nil {
				t.Fatalf("payload not valid JSON: %v", err)
			}
			if req.Method != "test.method" || req.ID != 1 {
				t.Fatalf("unexpected request envelope: %+v", req)
			}
		})
	}
}

func TestRPCErrorUnmarshal(t *testing.T) {
	cases := []struct {
		raw      string
		wantCode int
		wantMsg  string
	}{
		{`{"code":-32601,"message":"method not found"}`, -32601, "method not found"},
		{`"verbose transactions unsupported"`, 0, "verbose transactions unsupported"},
		{`{}`, 0, ""},
	}
	for _, tc := range cases {
		var e RPCError
		if err := e.UnmarshalJSON([]byte(tc.raw)); err != nil {
			t.Fatalf("unexpected unmarshal error for %q: %v", tc.raw, err)
		}
		if e.Code != tc.wantCode || e.Message != tc.wantMsg {
			t.Fatalf("raw=%q: got code=%d msg=%q, want code=%d msg=%q",
				tc.raw, e.Code, e.Message, tc.wantCode, tc.wantMsg)
		}
	}

	// Garbage input should return an error.
	var e RPCError
	if err := e.UnmarshalJSON([]byte("[]")); err == nil {
		t.Fatal("expected error for non-object/non-string RPCError input")
	}
}

func TestRPCErrorErrorString(t *testing.T) {
	e := RPCError{Code: -32602, Message: "bad params"}
	if got := e.Error(); !strings.Contains(got, "-32602") || !strings.Contains(got, "bad params") {
		t.Fatalf("Error() string missing fields: %s", got)
	}
}
