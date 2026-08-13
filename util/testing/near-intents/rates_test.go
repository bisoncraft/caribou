package nearintentsmock

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestFetchFiatRates exercises the /api/user rate fetch against a fake
// caribou app server response.
func TestFetchFiatRates(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/user" {
			http.NotFound(w, r)
			return
		}
		w.Write([]byte(`{"fiatRates":{"0":64123.5,"397":6.25},"assets":{}}`))
	}))
	defer srv.Close()

	rates, err := fetchFiatRates(context.Background(), srv.URL)
	if err != nil {
		t.Fatalf("fetchFiatRates: %v", err)
	}
	if rates[0] != 64123.5 || rates[397] != 6.25 {
		t.Fatalf("rates = %v", rates)
	}
}
