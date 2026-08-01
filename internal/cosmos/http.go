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
	errorBodyLimit  = 4096           // 4 KiB cap on buffered non-2xx response bodies
)

// grpcGatewayRoutingNotFoundMessage is the fixed message grpc-gateway's
// default routing error handler emits when no registered pattern matches
// the request path at all (see runtime.DefaultRoutingErrorHandler). It is
// always exactly this string with an empty details list, regardless of
// which Cosmos SDK module or version is involved, which is what makes it
// possible to tell apart from an app-level "resource not found" error that
// also happens to use gRPC code 5.
const grpcGatewayRoutingNotFoundMessage = "Not Found"

// grpcGatewayError is the {"code":...,"message":...,"details":[...]}  shape
// grpc-gateway uses for both its generic routing-miss error and any
// application error surfaced by a Cosmos SDK query handler.
type grpcGatewayError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Details []any  `json:"details"`
}

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

// IsRouteNotFound reports whether err represents an HTTP 404 that means the
// queried route itself does not exist on this endpoint - either because a
// plain-text 404 came from an HTTP router/proxy in front of the app, or
// because grpc-gateway's own generic routing-miss error fired (no pattern
// matched the request path at all). Only this should trigger falling back
// to the next binding.
//
// It must NOT report true for an application-level 404 where a Cosmos SDK
// query handler ran, matched the route, and legitimately could not find the
// resource being queried (e.g. "proposal 42 doesn't exist"). Both cases can
// arrive as JSON with the same gRPC code (5 = NotFound), so JSON-ness alone
// cannot tell them apart; the routing-miss body is always the fixed
// {"code":5,"message":"Not Found","details":[]}, while a resource miss
// carries a handler-specific message identifying what was looked up.
//
// A body that looks like JSON (starts with '{') but fails to parse is
// treated as a preserved application error rather than a route miss: the
// fixed routing-miss body is only a few dozen bytes and can never be cut by
// the errorBodyLimit truncation in DoJSON, so a parse failure here can only
// mean a legitimate, likely larger, app error got truncated.
func IsRouteNotFound(err error) bool {
	var statusErr *HTTPStatusError
	if !errors.As(err, &statusErr) || statusErr.StatusCode != http.StatusNotFound {
		return false
	}
	body := strings.TrimSpace(statusErr.Body)
	if body == "" {
		return true
	}
	if !strings.HasPrefix(body, "{") {
		return true
	}
	var gwErr grpcGatewayError
	if json.Unmarshal([]byte(body), &gwErr) != nil {
		return false
	}
	return gwErr.Code == 5 && gwErr.Message == grpcGatewayRoutingNotFoundMessage && len(gwErr.Details) == 0
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
		body, _ := io.ReadAll(io.LimitReader(resp.Body, errorBodyLimit))
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
