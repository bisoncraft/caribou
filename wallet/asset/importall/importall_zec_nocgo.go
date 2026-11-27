//go:build zec && !cgo

package importall

// The zec build tag requires cgo: zeclib links its Rust library through cgo,
// so a build with -tags zec needs CGO_ENABLED=1 and a C toolchain. Without
// cgo the build fails here, rather than producing a binary that was asked for
// Zcash and lacks it. The undefined identifier below names the requirement,
// so the compiler's error line carries it.
var _ = zecBuildTagRequiresCgoSetCGO_ENABLED1AndACToolchain
