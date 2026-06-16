// Copyright (c) 2024 The Bison Wallet developers
// Copyright (c) The go-electrum-client authors
// Use of this source code is governed by an ISC
// license that can be found in the LICENSE file.

package electrum

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
)

// positional is the wire shape for positional JSON-RPC arguments.
type positional []any

type rpcRequest struct {
	Jsonrpc string          `json:"jsonrpc"`
	ID      uint64          `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

// RPCError represents a JSON-RPC error object as returned by an electrumx
// server. Some servers also return a bare string in place of the error object,
// which UnmarshalJSON tolerates.
type RPCError struct {
	Code    int    `json:"code,omitempty"`
	Message string `json:"message,omitempty"`
}

// UnmarshalJSON accepts either the canonical {code, message} object or a bare
// string, the latter observed from at least one production server.
func (e *RPCError) UnmarshalJSON(b []byte) error {
	var obj struct {
		Code    int    `json:"code"`
		Message string `json:"message"`
	}
	if err := json.Unmarshal(b, &obj); err == nil {
		e.Code = obj.Code
		e.Message = obj.Message
		return nil
	}
	var s string
	if err := json.Unmarshal(b, &s); err == nil {
		e.Code = 0
		e.Message = s
		return nil
	}
	return errors.New("electrum: cannot parse RPCError from server")
}

// Error satisfies the error interface.
func (e RPCError) Error() string {
	return fmt.Sprintf("electrum: code %d: %q", e.Code, e.Message)
}

// rpcResponse covers both responses and notifications. Notifications have a
// non-empty Method; responses have an ID matching a pending request.
type rpcResponse struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Result json.RawMessage `json:"result"`
	Error  *RPCError       `json:"error"`
}

// ntfnPayload is the inner shape of a notification — params is the only field
// of interest beyond the method name (already captured in rpcResponse).
type ntfnPayload struct {
	Params json.RawMessage `json:"params"`
}

// prepareRequest marshals a JSON-RPC request. args must be a slice or struct
// (or pointer to struct); a nil args is sent as `[]`.
func prepareRequest(id uint64, method string, args any) ([]byte, error) {
	if args == nil {
		args = []json.RawMessage{}
	}
	switch rt := reflect.TypeOf(args); rt.Kind() {
	case reflect.Struct, reflect.Slice:
	case reflect.Ptr:
		if rt.Elem().Kind() != reflect.Struct {
			return nil, fmt.Errorf("electrum: invalid arg type %v, want slice or struct", rt)
		}
	default:
		return nil, fmt.Errorf("electrum: invalid arg type %v, want slice or struct", rt)
	}
	params, err := json.Marshal(args)
	if err != nil {
		return nil, fmt.Errorf("electrum: marshal request params: %w", err)
	}
	return json.Marshal(&rpcRequest{
		Jsonrpc: "2.0",
		ID:      id,
		Method:  method,
		Params:  params,
	})
}
