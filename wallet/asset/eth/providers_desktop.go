//go:build !mobile

package eth

// limitDefaultProviders returns the built-in provider list unchanged. A desktop
// connects to all of them, which is the redundancy the multi-provider client was
// built for: failover without a reconnect, and a second opinion on a header.
// See providers_mobile.go for the phone build.
func limitDefaultProviders(endpoints []string) []string {
	return endpoints
}
