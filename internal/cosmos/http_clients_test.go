package cosmos

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestRPCClientReadOnlyAndResult(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var request map[string]any
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error(err)
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"jsonrpc": "2.0", "id": request["id"], "result": map[string]any{"ok": true}})
	}))
	defer upstream.Close()
	client, err := NewRPCClient(upstream.URL, NewHTTPClient(time.Second, 1024))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Call(context.Background(), "status", map[string]any{})
	if err != nil || result.(map[string]any)["ok"] != true {
		t.Fatalf("Call() = %#v, %v", result, err)
	}
	for _, method := range []string{"broadcast_tx_sync", "unsafe_flush_mempool", "unknown_custom_method"} {
		if _, err := client.Call(context.Background(), method, nil); errorCode(err) != CodeInvalidInput {
			t.Fatalf("method %q error = %v", method, err)
		}
	}
	if _, err := client.Block(context.Background(), "0"); errorCode(err) != CodeInvalidInput {
		t.Fatalf("invalid block height error = %v", err)
	}
}

func TestLCDClientRestrictsURLAndSize(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/large" {
			_, _ = w.Write([]byte(`{"data":"` + string(make([]byte, 256)) + `"}`))
			return
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"path": r.URL.Path, "value": r.URL.Query().Get("key")})
	}))
	defer upstream.Close()
	client, err := NewLCDClient(upstream.URL, NewHTTPClient(time.Second, 64))
	if err != nil {
		t.Fatal(err)
	}
	result, err := client.Get(context.Background(), "/safe", map[string]string{"key": "value"})
	if err != nil || result.(map[string]any)["path"] != "/safe" {
		t.Fatalf("Get() = %#v, %v", result, err)
	}
	for _, unsafePath := range []string{"https://evil.example/path", "//evil.example/path", "relative", "/../admin", "/%2e%2e/admin"} {
		if _, err := client.Get(context.Background(), unsafePath, nil); errorCode(err) != CodeInvalidInput {
			t.Fatalf("path %q error = %v", unsafePath, err)
		}
	}
	if _, err := client.Get(context.Background(), "/large", nil); errorCode(err) != CodeResponseTooLarge {
		t.Fatalf("large response error = %v", err)
	}
}

func TestRedactedURL(t *testing.T) {
	got := RedactedURL("https://user:secret@example.com/base?token=secret#fragment")
	if got != "https://example.com/base" {
		t.Fatalf("RedactedURL() = %q", got)
	}
}

func errorCode(err error) ErrorCode {
	code, _ := ErrorDetails(err)
	return code
}
