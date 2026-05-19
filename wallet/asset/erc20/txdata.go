// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package erc20

import (
	"bytes"
	"fmt"
	"math/big"

	"github.com/ethereum/go-ethereum/common"
)

// parseCallData decodes the named method from the calldata against the package
// ERC20ABI and verifies the argument count. It also re-encodes the decoded
// arguments and compares with the input bytes to catch any extra/malformed
// payload that the decoder might silently ignore.
func parseCallData(method string, data []byte, expArgs int) ([]any, error) {
	if len(data) < 4 {
		return nil, fmt.Errorf("invalid call data, incomplete method signature (%d bytes < 4)", len(data))
	}
	argdata := data[4:]
	if len(argdata)%32 != 0 {
		return nil, fmt.Errorf("invalid call data; length should be a multiple of 32 bytes (was %d)", len(argdata))
	}
	m, err := ERC20ABI.MethodById(data[:4])
	if err != nil {
		return nil, fmt.Errorf("unable to parse call data: %v", err)
	}
	if m.RawName != method {
		return nil, fmt.Errorf("expected %v function but got %v", method, m.RawName)
	}
	args, err := m.Inputs.UnpackValues(argdata)
	if err != nil {
		return nil, fmt.Errorf("signature %q matches, but arguments mismatch: %v", m.String(), err)
	}
	if len(args) != expArgs {
		return nil, fmt.Errorf("wrong number of arguments. wanted %d, got %d", expArgs, len(args))
	}
	encoded, err := m.Inputs.PackValues(args)
	if err != nil {
		return nil, err
	}
	if !bytes.Equal(encoded, argdata) {
		return nil, fmt.Errorf("WARNING! Supplied data is stuffed with extra data. want %s, got %s",
			common.Bytes2Hex(encoded), common.Bytes2Hex(argdata))
	}
	return args, nil
}

// ParseTransferFromData parses the calldata used to call the transferFrom
// method of an ERC20 contract.
func ParseTransferFromData(data []byte) (sender, recipient common.Address, amount *big.Int, err error) {
	args, err := parseCallData("transferFrom", data, 3)
	if err != nil {
		return common.Address{}, common.Address{}, nil, fmt.Errorf("unable to parse call data: %v", err)
	}

	sender, ok := args[0].(common.Address)
	if !ok {
		return common.Address{}, common.Address{}, nil, fmt.Errorf("expected first arg of type common.Address but got %T", args[0])
	}

	recipient, ok = args[1].(common.Address)
	if !ok {
		return common.Address{}, common.Address{}, nil, fmt.Errorf("expected second arg of type common.Address but got %T", args[1])
	}

	amount, ok = args[2].(*big.Int)
	if !ok {
		return common.Address{}, common.Address{}, nil, fmt.Errorf("expected third arg of type *big.Int but got %T", args[2])
	}

	return sender, recipient, amount, nil
}

// ParseTransferData parses the calldata used to call the transfer method of an
// ERC20 contract.
func ParseTransferData(data []byte) (common.Address, *big.Int, error) {
	args, err := parseCallData("transfer", data, 2)
	if err != nil {
		return common.Address{}, nil, fmt.Errorf("unable to parse call data: %v", err)
	}

	recipient, ok := args[0].(common.Address)
	if !ok {
		return common.Address{}, nil, fmt.Errorf("expected first arg of type common.Address but got %T", args[0])
	}
	value, ok := args[1].(*big.Int)
	if !ok {
		return common.Address{}, nil, fmt.Errorf("expected second arg of type *big.Int but got %T", args[1])
	}

	return recipient, value, nil
}
