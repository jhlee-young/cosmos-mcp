package cosmos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestResolverFallsBackOnMissingLCDRouteAndCaches(t *testing.T) {
	var currentCalls atomic.Int32
	var legacyCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/current":
			currentCalls.Add(1)
			http.NotFound(w, r)
		case "/legacy":
			legacyCalls.Add(1)
			_ = json.NewEncoder(w).Encode(map[string]any{"version": "legacy"})
		default:
			t.Fatal("unexpected path", r.URL.Path)
		}
	}))
	defer upstream.Close()
	lcd, err := NewLCDClient(upstream.URL, NewHTTPClient(time.Second, 1024))
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(nil, lcd, nil)
	bindings := []Binding{{Name: "current", LCDPath: "/current"}, {Name: "legacy", LCDPath: "/legacy"}}
	for range 2 {
		result, resolveErr := resolver.Resolve(context.Background(), "proposal", bindings)
		if resolveErr != nil || result.Binding != "/legacy" {
			t.Fatalf("Resolve() = %#v, %v", result, resolveErr)
		}
	}
	if currentCalls.Load() != 1 || legacyCalls.Load() != 2 {
		t.Fatalf("calls current=%d legacy=%d", currentCalls.Load(), legacyCalls.Load())
	}
}

func TestResolverDoesNotFallbackOnLCDServerError(t *testing.T) {
	var fallbackCalls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/current" {
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		fallbackCalls.Add(1)
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true})
	}))
	defer upstream.Close()
	lcd, err := NewLCDClient(upstream.URL, NewHTTPClient(time.Second, 1024))
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(nil, lcd, nil)
	_, err = resolver.Resolve(context.Background(), "proposal", []Binding{{LCDPath: "/current"}, {LCDPath: "/legacy"}})
	if errorCode(err) != CodeUpstreamError || fallbackCalls.Load() != 0 {
		t.Fatalf("Resolve() error=%v fallback calls=%d", err, fallbackCalls.Load())
	}
}

func TestResolverUnsupportedCapabilityIncludesAttempts(t *testing.T) {
	resolver := NewResolver(nil, nil, nil)
	_, err := resolver.Resolve(context.Background(), "account", []Binding{{Name: "grpc:account", GRPCMethod: "/cosmos.auth.v1beta1.Query/Account"}})
	if errorCode(err) != CodeUnsupportedCapability {
		t.Fatalf("Resolve() error = %v", err)
	}
	details := ErrorDetailsMap(err)
	if details["capability"] != "account" {
		t.Fatalf("details = %#v", details)
	}
}

func TestResolverCachesUnsupportedCapabilityWithoutReattempting(t *testing.T) {
	var calls atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	lcd, err := NewLCDClient(upstream.URL, NewHTTPClient(time.Second, 1024))
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(nil, lcd, nil)
	bindings := []Binding{{Name: "a", LCDPath: "/a"}, {Name: "b", LCDPath: "/b"}}

	if _, err := resolver.Resolve(context.Background(), "unsupported_capability", bindings); errorCode(err) != CodeUnsupportedCapability {
		t.Fatalf("Resolve() first call error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected both bindings probed once, calls = %d", calls.Load())
	}

	if _, err := resolver.Resolve(context.Background(), "unsupported_capability", bindings); errorCode(err) != CodeUnsupportedCapability {
		t.Fatalf("Resolve() second call error = %v", err)
	}
	if calls.Load() != 2 {
		t.Fatalf("expected cached unsupported result to skip re-probing, calls = %d", calls.Load())
	}
}

func TestResolverBudgetBoundsTotalResolveTime(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(150 * time.Millisecond)
		http.NotFound(w, r)
	}))
	defer upstream.Close()
	lcd, err := NewLCDClient(upstream.URL, NewHTTPClient(2*time.Second, 1024))
	if err != nil {
		t.Fatal(err)
	}
	resolver := NewResolver(nil, lcd, nil)
	resolver.Budget = 50 * time.Millisecond

	started := time.Now()
	_, err = resolver.Resolve(context.Background(), "budget_test", []Binding{{LCDPath: "/a"}, {LCDPath: "/b"}, {LCDPath: "/c"}})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatalf("Resolve() expected an error once the budget was exhausted")
	}
	if elapsed > 300*time.Millisecond {
		t.Fatalf("Resolve() took %s, want well under the 450ms it would take to run all three 150ms bindings sequentially", elapsed)
	}
}

func TestLCDGetMultiPreservesRepeatedValues(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{"events": r.URL.Query()["events"]})
	}))
	defer upstream.Close()
	lcd, err := NewLCDClient(upstream.URL, NewHTTPClient(time.Second, 1024))
	if err != nil {
		t.Fatal(err)
	}
	result, err := lcd.GetMulti(context.Background(), "/txs", map[string][]string{"events": {"a=1", "b=2"}})
	if err != nil || len(result.(map[string]any)["events"].([]any)) != 2 {
		t.Fatalf("GetMulti() = %#v, %v", result, err)
	}
}
