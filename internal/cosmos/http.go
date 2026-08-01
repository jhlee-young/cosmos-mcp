package cosmos

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"time"
)

type HTTPClient struct {
	client   *http.Client
	timeout  time.Duration
	maxBytes int64
}

func NewHTTPClient(timeout time.Duration, maxBytes int64) *HTTPClient {
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
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
		return NewError(CodeUpstreamError, fmt.Sprintf("upstream returned HTTP %d", resp.StatusCode), nil)
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
