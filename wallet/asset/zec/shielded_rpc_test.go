// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import "testing"

func TestZListAccounts(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZListAccounts, []zListAccountsResult{{Number: 1}})
	accts, err := zListAccounts(w)
	if err != nil {
		t.Fatalf("zListAccounts error: %v", err)
	}
	if len(accts) != 1 || accts[0].Number != 1 {
		t.Fatalf("unexpected result: %+v", accts)
	}

	cl.queueResponse(methodZListAccounts, errTest)
	if _, err := zListAccounts(w); err == nil {
		t.Fatalf("expected error")
	}
}

func TestZGetNewAccount(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZGetNewAccount, &struct {
		Number uint32 `json:"account"`
	}{Number: 3})
	n, err := zGetNewAccount(w)
	if err != nil {
		t.Fatalf("zGetNewAccount error: %v", err)
	}
	if n != 3 {
		t.Fatalf("wrong account number: %d", n)
	}

	cl.queueResponse(methodZGetNewAccount, errTest)
	if _, err := zGetNewAccount(w); err == nil {
		t.Fatalf("expected error")
	}
}

func TestZGetAddressForAccount(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZGetAddressForAccount, &zGetAddressForAccountResult{Account: 1, Address: tAddr})
	res, err := zGetAddressForAccount(w, 1, []string{"orchard"})
	if err != nil {
		t.Fatalf("zGetAddressForAccount error: %v", err)
	}
	if res.Address != tAddr {
		t.Fatalf("unexpected address: %s", res.Address)
	}
}

func TestZGetUnifiedReceivers(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZListUnifiedReceivers, &unifiedReceivers{Transparent: tAddr})
	res, err := zGetUnifiedReceivers(w, "unifiedaddr")
	if err != nil {
		t.Fatalf("zGetUnifiedReceivers error: %v", err)
	}
	if res.Transparent != tAddr {
		t.Fatalf("unexpected result: %+v", res)
	}
}

func TestZGetBalanceForAccount(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	resp := zAccountBalance{}
	resp.Pools.Orchard.ValueZat = 100
	resp.Pools.Transparent.ValueZat = 200
	resp.Pools.Sapling.ValueZat = 300
	cl.queueResponse(methodZGetBalanceForAccount, &resp)

	bal, err := zGetBalanceForAccount(w, 0, 1)
	if err != nil {
		t.Fatalf("zGetBalanceForAccount error: %v", err)
	}
	if bal.Orchard != 100 || bal.Transparent != 200 || bal.Sapling != 300 {
		t.Fatalf("unexpected balances: %+v", bal)
	}

	cl.queueResponse(methodZGetBalanceForAccount, errTest)
	if _, err := zGetBalanceForAccount(w, 0, 1); err == nil {
		t.Fatalf("expected error")
	}
}

func TestZGetNotesCount(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZGetNotesCount, &zNotesCount{Orchard: 5})
	counts, err := zGetNotesCount(w)
	if err != nil {
		t.Fatalf("zGetNotesCount error: %v", err)
	}
	if counts.Orchard != 5 {
		t.Fatalf("unexpected counts: %+v", counts)
	}
}

func TestZValidateAddress(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZValidateAddress, &zValidateAddressResult{IsValid: true})
	res, err := zValidateAddress(w, tAddr)
	if err != nil {
		t.Fatalf("zValidateAddress error: %v", err)
	}
	if !res.IsValid {
		t.Fatalf("expected valid address")
	}
}

func TestSingleSendManyRecipient(t *testing.T) {
	recips := singleSendManyRecipient(tAddr, 1e8)
	if len(recips) != 1 || recips[0].Address != tAddr || recips[0].Amount != 1 {
		t.Fatalf("unexpected recipients: %+v", recips)
	}
}

func TestZSendMany(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZSendMany, "opid")
	opID, err := zSendMany(w, tAddr, singleSendManyRecipient(tAddr, 1e8), FullPrivacy)
	if err != nil {
		t.Fatalf("zSendMany error: %v", err)
	}
	if opID != "opid" {
		t.Fatalf("unexpected op id: %s", opID)
	}
}

func TestZGetOperationResult(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZGetOperationResult, []*operationStatus{{Status: "success"}})
	res, err := zGetOperationResult(w, "opid")
	if err != nil {
		t.Fatalf("zGetOperationResult error: %v", err)
	}
	if res.Status != "success" {
		t.Fatalf("unexpected status: %s", res.Status)
	}

	// Empty results is a distinct sentinel error.
	cl.queueResponse(methodZGetOperationResult, []*operationStatus{})
	if _, err := zGetOperationResult(w, "opid"); err != ErrEmptyOpResults {
		t.Fatalf("expected ErrEmptyOpResults, got %v", err)
	}

	cl.queueResponse(methodZGetOperationResult, errTest)
	if _, err := zGetOperationResult(w, "opid"); err == nil {
		t.Fatalf("expected error")
	}
}

func TestZListUnspent(t *testing.T) {
	w, cl, shutdown := tNewWallet()
	defer shutdown()
	defer cl.checkEmptiness(t)

	cl.queueResponse(methodZListUnspent, []*zListUnspentResult{{TxID: tTxID}})
	res, err := zListUnspent(w)
	if err != nil {
		t.Fatalf("zListUnspent error: %v", err)
	}
	if len(res) != 1 || res[0].TxID != tTxID {
		t.Fatalf("unexpected result: %+v", res)
	}

	cl.queueResponse(methodZListUnspent, errTest)
	if _, err := zListUnspent(w); err == nil {
		t.Fatalf("expected error")
	}
}
