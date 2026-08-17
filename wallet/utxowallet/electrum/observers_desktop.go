//go:build !mobile

package electrum

// maxObservers bounds the observer connections per network. Three gives the
// divergence and lag checks several independent opinions to weigh the wallet
// server against, at a socket cost a desktop does not notice. See
// observers_mobile.go for the phone build.
const maxObservers = 3
