package broadcast

import (
	"errors"
	"testing"
)

func TestIsAlreadyBroadcastErr(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"mempool", errors.New("txn-already-in-mempool"), true},
		{"known", errors.New("txn-already-known"), true},
		{"blockchain", errors.New("already in block chain"), true},
		{"exists", errors.New("transaction already exists"), true},
		{"have", errors.New("already have transaction"), true},
		{"tx exists", errors.New("tx already exists"), true},
		{"in mempool", errors.New("Already in the mempool"), true},
		{"unrelated", errors.New("insufficient funds"), false},
		{"wrapped", errors.New("rpc error: txn-already-in-mempool"), true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := IsAlreadyBroadcastErr(tt.err); got != tt.want {
				t.Errorf("IsAlreadyBroadcastErr() = %v, want %v", got, tt.want)
			}
		})
	}
}
