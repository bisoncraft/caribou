// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

package zec

import (
	"errors"
	"testing"
)

func TestErrorType(t *testing.T) {
	wrapped := errors.New("underlying")
	err := newError(errBadInput, "bad input: %w", wrapped)

	var e *Error
	if !errors.As(err, &e) {
		t.Fatalf("newError did not produce an *Error")
	}
	if e.Error() != "bad input: underlying" {
		t.Errorf("Error() = %q", e.Error())
	}
	if *e.Code() != errBadInput {
		t.Errorf("Code() = %d, want %d", *e.Code(), errBadInput)
	}
	if !errors.Is(e.Unwrap(), wrapped) {
		t.Errorf("Unwrap() did not return the wrapped error")
	}
	if !errorHasCode(err, errBadInput) {
		t.Errorf("errorHasCode should be true for errBadInput")
	}
	if errorHasCode(err, errMaxLock) {
		t.Errorf("errorHasCode should be false for errMaxLock")
	}

	ce := codedError(errFunding, wrapped)
	if !errorHasCode(ce, errFunding) {
		t.Errorf("codedError should carry errFunding")
	}
	if !errors.Is(ce, wrapped) {
		t.Errorf("codedError should unwrap to the underlying error")
	}

	if errorHasCode(wrapped, errBadInput) {
		t.Errorf("errorHasCode should be false for a plain error")
	}
}
