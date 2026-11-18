// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package rhea

import (
	"bytes"
	"fmt"
)

// RecipientMsg is a 1Click customRecipientMsg for an MCA supply. 1Click
// delivers the tokens with ft_transfer_call carrying it; the receiving
// contract (the MCA, or the account manager for a new MCA) runs the named
// account-manager template for the listed wallets. Signatures is set only
// when creating an MCA: one creation signature per wallet.
type RecipientMsg struct {
	Wallets    []Wallet
	Template   string
	Signatures []string
}

// String encodes the message with members in the RHEA frontend's order:
// {"w":[...],"b":{"r":"<template>"},"s":[...]}, "s" only when set.
func (m *RecipientMsg) String() string {
	var b bytes.Buffer
	o := newObj(&b)
	o.raw("w", func() error {
		b.WriteByte('[')
		for i, w := range m.Wallets {
			if i > 0 {
				b.WriteByte(',')
			}
			wb, err := w.MarshalJSON()
			if err != nil {
				return err
			}
			b.Write(wb)
		}
		b.WriteByte(']')
		return nil
	})
	o.raw("b", func() error {
		r := newObj(&b)
		r.str("r", m.Template)
		return r.close()
	})
	if len(m.Signatures) > 0 {
		o.raw("s", func() error {
			b.WriteByte('[')
			for i, s := range m.Signatures {
				if i > 0 {
					b.WriteByte(',')
				}
				if err := writeJSONString(&b, s); err != nil {
					return err
				}
			}
			b.WriteByte(']')
			return nil
		})
	}
	_ = o.close() // string and wallet encoding cannot fail
	return b.String()
}

// SupplyMsg is the recipient message for supplying to an existing MCA
// bound to id. The template supplies with msg "" (plain supply).
func SupplyMsg(id Identity) string {
	return (&RecipientMsg{Wallets: []Wallet{id.Wallet()}, Template: TemplateBurrowSupply}).String()
}

// RegisterAndSupplyMsg is the recipient message, sent to the account
// manager, that creates an MCA bound to id and supplies what remains after
// the creation fee. creationSig must be id's signature over
// CreationMessage(id); it is verified here.
func RegisterAndSupplyMsg(id Identity, creationSig string) (string, error) {
	signer, err := RecoverPersonal(CreationMessage(id), creationSig)
	if err != nil {
		return "", fmt.Errorf("bad creation signature: %w", err)
	}
	if signer != id {
		return "", fmt.Errorf("creation signature is by %s, not %s", signer, id)
	}
	return (&RecipientMsg{
		Wallets:    []Wallet{id.Wallet()},
		Template:   TemplateBurrowRegisterAndSupply,
		Signatures: []string{creationSig},
	}).String(), nil
}

// ParseRecipientMsg decodes a recipient message.
func ParseRecipientMsg(s string) (*RecipientMsg, error) {
	var v struct {
		W []Wallet `json:"w"`
		B struct {
			R string `json:"r"`
		} `json:"b"`
		S []string `json:"s"`
	}
	if err := decodeStrict([]byte(s), &v); err != nil {
		return nil, fmt.Errorf("error decoding recipient message: %w", err)
	}
	for i, sig := range v.S {
		if sig == "" {
			return nil, fmt.Errorf("recipient message signature %d is empty or null", i)
		}
	}
	return &RecipientMsg{Wallets: v.W, Template: v.B.R, Signatures: v.S}, nil
}
