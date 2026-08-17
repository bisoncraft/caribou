//go:build !mobile

package eth

// wantProviderLimit is how many of the built-in providers a desktop build keeps:
// all five of the test list, since it does not cap at all.
const wantProviderLimit = 5
