package cosmos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func firstOrNil(errs []error) error {
	if len(errs) == 0 {
		return nil
	}
	return errs[0]
}

func TestLCDSendsHeightHeaderOnlyWhenPinned(t *testing.T) {
	var got string
	var present bool
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get(HeightHeader)
		_, present = r.Header[http.CanonicalHeaderKey(HeightHeader)]
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()
	lcd, err := NewLCDClient(upstream.URL, NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}

	if _, err := lcd.Get(WithHeight(context.Background(), "12345"), "/a", nil); err != nil {
		t.Fatal(err)
	}
	if got != "12345" {
		t.Fatalf("%s = %q, want 12345", HeightHeader, got)
	}

	if _, err := lcd.Get(context.Background(), "/a", nil); err != nil {
		t.Fatal(err)
	}
	if present {
		t.Fatalf("%s must be absent when no height is pinned, got %q", HeightHeader, got)
	}
}

func TestGRPCSendsHeightMetadataOnlyWhenPinned(t *testing.T) {
	client, stop := newTestGRPCClient(t, true)
	defer stop()

	result, err := client.Query(WithHeight(context.Background(), "987"), "/cosmos.mcp.test.v1.Query/Echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if height := result.(map[string]any)["height"]; height != "987" {
		t.Fatalf("received height metadata = %v, want 987", height)
	}

	result, err = client.Query(context.Background(), "/cosmos.mcp.test.v1.Query/Echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if height := result.(map[string]any)["height"]; height != "" {
		t.Fatalf("received height metadata = %v, want empty", height)
	}
}

// WithHeight is deliberately carried on the context so that a binding added
// later inherits it without opting in; an empty height must be a no-op rather
// than pinning to a bogus value.
func TestWithHeightIgnoresEmptyHeight(t *testing.T) {
	ctx := context.Background()
	if WithHeight(ctx, "") != ctx {
		t.Fatal("WithHeight(ctx, \"\") must return the original context")
	}
	if heightFrom(WithHeight(ctx, "7")) != "7" {
		t.Fatal("heightFrom did not round-trip the pinned height")
	}
}

// A node that pruned the requested height rejects the query. That rejection
// must surface to the caller, never silently downgrade to the next binding,
// which would answer from a different API and possibly a different height.
func TestResolverDoesNotFallbackOnGRPCNonUnimplementedCodes(t *testing.T) {
	for _, code := range []codes.Code{
		codes.InvalidArgument,
		codes.NotFound,
		codes.ResourceExhausted,
		codes.Unauthenticated,
		codes.PermissionDenied,
		codes.Internal,
	} {
		t.Run(code.String(), func(t *testing.T) {
			var fallbackCalls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				fallbackCalls.Add(1)
				_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
			}))
			defer upstream.Close()
			lcd, err := NewLCDClient(upstream.URL, NewHTTPClient(time.Second, 1<<20))
			if err != nil {
				t.Fatal(err)
			}
			client, stop := newTestGRPCClient(t, true, status.Error(code, "upstream refused"))
			defer stop()

			resolver := NewResolver(nil, lcd, client)
			_, err = resolver.Resolve(context.Background(), "echo", []Binding{
				{Name: "grpc:echo", GRPCMethod: "/cosmos.mcp.test.v1.Query/Echo", GRPCRequest: map[string]any{}},
				{Name: "lcd:echo", LCDPath: "/echo"},
			})
			if err == nil {
				t.Fatalf("Resolve() returned no error for gRPC %s", code)
			}
			if fallbackCalls.Load() != 0 {
				t.Fatalf("gRPC %s fell through to the LCD binding %d time(s)", code, fallbackCalls.Load())
			}
		})
	}
}

// Unimplemented is the one gRPC code that does mean "this chain does not have
// this API", so it must keep falling through to the next binding.
func TestResolverFallsBackOnGRPCUnimplemented(t *testing.T) {
	var fallbackCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		fallbackCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()
	lcd, err := NewLCDClient(upstream.URL, NewHTTPClient(time.Second, 1<<20))
	if err != nil {
		t.Fatal(err)
	}
	client, stop := newTestGRPCClient(t, true, status.Error(codes.Unimplemented, "not implemented"))
	defer stop()

	resolver := NewResolver(nil, lcd, client)
	result, err := resolver.Resolve(context.Background(), "echo", []Binding{
		{Name: "grpc:echo", GRPCMethod: "/cosmos.mcp.test.v1.Query/Echo", GRPCRequest: map[string]any{}},
		{Name: "lcd:echo", LCDPath: "/echo"},
	})
	if err != nil || result.Source != "lcd" || fallbackCalls.Load() != 1 {
		t.Fatalf("Resolve() = %#v, err=%v, lcd calls=%d", result, err, fallbackCalls.Load())
	}
}
