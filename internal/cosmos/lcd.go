package cosmos

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"path"
	"strings"
)

type LCDClient struct {
	base *url.URL
	http *HTTPClient
}

func NewLCDClient(rawURL string, httpClient *HTTPClient) (*LCDClient, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return nil, err
	}
	return &LCDClient{base: u, http: httpClient}, nil
}

func (c *LCDClient) Endpoint() string { return RedactedURL(c.base.String()) }

func (c *LCDClient) Get(ctx context.Context, requestPath string, query map[string]string) (any, error) {
	if requestPath == "" || !strings.HasPrefix(requestPath, "/") || strings.HasPrefix(requestPath, "//") {
		return nil, NewError(CodeInvalidInput, "LCD path must be an absolute path beginning with one slash", nil)
	}
	parsed, err := url.ParseRequestURI(requestPath)
	if err != nil || parsed.Host != "" || parsed.Scheme != "" {
		return nil, NewError(CodeInvalidInput, "LCD path must not be an absolute URL", err)
	}
	if parsed.RawQuery != "" {
		return nil, NewError(CodeInvalidInput, "put LCD query parameters in the query object", nil)
	}
	for _, segment := range strings.Split(parsed.Path, "/") {
		if segment == "." || segment == ".." {
			return nil, NewError(CodeInvalidInput, "LCD path must not contain traversal segments", nil)
		}
	}
	u := *c.base
	u.Path = path.Join(strings.TrimSuffix(c.base.Path, "/"), parsed.Path)
	u.RawPath = ""
	values := c.base.Query()
	for key, value := range query {
		if strings.TrimSpace(key) == "" {
			return nil, NewError(CodeInvalidInput, "LCD query parameter names must not be empty", nil)
		}
		if values.Has(key) {
			return nil, NewError(CodeInvalidInput, fmt.Sprintf("query parameter %q is fixed by the configured LCD endpoint and cannot be overridden", key), nil)
		}
		values.Set(key, value)
	}
	u.RawQuery = values.Encode()
	req, err := http.NewRequest(http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, NewError(CodeInvalidInput, "failed to build LCD request", err)
	}
	var result any
	if err := c.http.DoJSON(ctx, req, &result); err != nil {
		return nil, err
	}
	return result, nil
}

func (c *LCDClient) Balances(ctx context.Context, address, denom string) (any, error) {
	address = strings.TrimSpace(address)
	if address == "" || strings.ContainsAny(address, "/?# \\") {
		return nil, NewError(CodeInvalidInput, "address must be a non-empty bech32-style value", nil)
	}
	base := "/cosmos/bank/v1beta1/balances/" + url.PathEscape(address)
	query := map[string]string{}
	if denom != "" {
		base += "/by_denom"
		query["denom"] = denom
	}
	return c.Get(ctx, base, query)
}

func (c *LCDClient) Status(ctx context.Context) error {
	_, err := c.Get(ctx, "/cosmos/base/tendermint/v1beta1/node_info", nil)
	if err != nil {
		return fmt.Errorf("LCD health check: %w", err)
	}
	return nil
}
