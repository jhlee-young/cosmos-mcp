package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

const (
	defaultTimeout  = 15 * time.Second
	defaultMaxBytes = int64(4 << 20) // 4 MiB
)

type HTTPClient struct {
	client   *http.Client
	timeout  time.Duration
	maxBytes int64
}

type HTTPStatusError struct {
	StatusCode int
	Body       string
}

func (e *HTTPStatusError) Error() string {
	return fmt.Sprintf("upstream returned HTTP %d", e.StatusCode)
}

// IsRouteNotFound reports whether err represents an HTTP 404 whose body is
// plain text from an HTTP router/proxy (route does not exist), as opposed to
// a JSON error payload from the Cosmos SDK app itself (e.g. grpc-gateway's
// {"code":...,"message":...} for a resource that legitimately wasn't found).
// Only the former should trigger falling back to the next binding.
func IsRouteNotFound(err error) bool {
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
		return false
	}
	body := strings.TrimSpace(statusErr.Body)
	if body == "" {
		return true
	}
	var jsonBody any
	return json.Unmarshal([]byte(body), &jsonBody) != nil
}

func NewHTTPClient(timeout time.Duration, maxBytes int64) *HTTPClient {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.MaxIdleConns = 32
	transport.MaxIdleConnsPerHost = 8
	return &HTTPClient{
		client: &http.Client{
			Transport: transport,
			Timeout:   timeout,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		timeout:  timeout,
		maxBytes: maxBytes,
	}
}

func (c *HTTPClient) DoJSON(ctx context.Context, req *http.Request, out any) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := c.client.Do(req)
	if err != nil {
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return NewError(CodeUpstreamTimeout, "upstream request timed out", err)
		}
		var netErr net.Error
		if errors.As(err, &netErr) {
			return NewError(CodeEndpointUnavailable, "endpoint is unavailable", err)
		}
		return NewError(CodeEndpointUnavailable, "endpoint request failed", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
		statusErr := &HTTPStatusError{StatusCode: resp.StatusCode, Body: string(body)}
		return NewError(CodeUpstreamError, statusErr.Error(), statusErr)
	}
	limited := io.LimitReader(resp.Body, c.maxBytes+1)
	data, err := io.ReadAll(limited)
	if err != nil {
		return NewError(CodeUpstreamError, "failed to read upstream response", err)
	}
	if int64(len(data)) > c.maxBytes {
		return NewError(CodeResponseTooLarge, "upstream response exceeded the configured size limit", nil)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return NewError(CodeUpstreamError, "upstream returned invalid JSON", err)
	}
	return nil
}
