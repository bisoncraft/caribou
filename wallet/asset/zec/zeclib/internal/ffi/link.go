// This code is available on the terms of the project LICENSE.md file,
// also available online at https://blueoakcouncil.org/license/1.0.0.

//go:build zec

package ffi

// The Rust backend is linked statically, so a binary built with it has no
// extra shared library to ship. The trailing system libraries are the ones
// rustc lists for the static library (cargo rustc -- --print
// native-static-libs).

/*
#cgo CFLAGS: -I${SRCDIR}
#cgo linux,amd64 LDFLAGS: ${SRCDIR}/../../lib/linux-amd64/libzeclib.a
#cgo linux,arm64 LDFLAGS: ${SRCDIR}/../../lib/linux-arm64/libzeclib.a
#cgo linux LDFLAGS: -lgcc_s -lutil -lrt -lpthread -lm -ldl
*/
import "C"
