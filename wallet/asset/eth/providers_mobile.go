//go:build mobile

package eth

// maxMobileProviders caps the built-in provider count per EVM chain on mobile.
// The default lists run three or four deep, and every entry is dialed and
// verified with a chain ID and a header request at connect, then held as an
// open connection. The cap bounds that setup cost and connection count on a
// phone.
//
// Deliberately two and not one. The point of the multi-provider client is that a
// dead or rate-limited endpoint costs nothing, since another is already
// connected; at one provider its failure becomes an outage that a reconnect has
// to resolve. Two keeps that property at half the connections.
const maxMobileProviders = 2

// limitDefaultProviders trims the built-in provider list to maxMobileProviders,
// keeping the first entries because the lists are written in preference order.
//
// Only the built-in list is trimmed. An explicit providers setting is passed
// through untouched: someone who listed their own endpoints said how many to
// use, and silently dropping some would be answering a question they already
// answered.
func limitDefaultProviders(endpoints []string) []string {
	if len(endpoints) <= maxMobileProviders {
		return endpoints
	}
	// Copy rather than reslice; the caller's list is package-level state in the
	// chain packages, and a subslice would alias it.
	return append([]string(nil), endpoints[:maxMobileProviders]...)
}
