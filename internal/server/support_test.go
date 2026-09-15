package server

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/jhlee-young/cosmos-mcp/internal/cosmos"
)

// newLCDServer builds a Server backed by a loopback LCD endpoint. Tests reach
// for this rather than a public node so a query's routing and normalization can
// be asserted exactly.
func newLCDServer(t *testing.T, handler http.HandlerFunc) *Server {
	t.Helper()
	upstream := httptest.NewServer(handler)
	t.Cleanup(upstream.Close)
	lcd, err := cosmos.NewLCDClient(upstream.URL, cosmos.NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	return &Server{lcd: lcd, query: cosmos.NewResolver(nil, lcd, nil)}
}
