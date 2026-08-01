package config

import (
	"errors"
	"flag"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

const (
	defaultTimeout  = 15 * time.Second
	defaultMaxBytes = int64(4 << 20)
)

type Config struct {
	RPCURL           string
	GRPCTarget       string
	GRPCInsecure     bool
	LCDURL           string
	Timeout          time.Duration
	MaxResponseBytes int64
}

type Getter func(string) string

func Parse(args []string, getenv Getter) (Config, error) {
	if getenv == nil {
		getenv = func(string) string { return "" }
	}
	cfg := Config{
		RPCURL:           getenv("COSMOS_MCP_RPC_URL"),
		GRPCTarget:       getenv("COSMOS_MCP_GRPC_TARGET"),
		LCDURL:           getenv("COSMOS_MCP_LCD_URL"),
		Timeout:          defaultTimeout,
		MaxResponseBytes: defaultMaxBytes,
	}
	if raw := getenv("COSMOS_MCP_GRPC_INSECURE"); raw != "" {
		value, err := strconv.ParseBool(raw)
		if err != nil {
			return Config{}, fmt.Errorf("COSMOS_MCP_GRPC_INSECURE: %w", err)
		}
		cfg.GRPCInsecure = value
	}
	if raw := getenv("COSMOS_MCP_TIMEOUT"); raw != "" {
		value, err := time.ParseDuration(raw)
		if err != nil {
			return Config{}, fmt.Errorf("COSMOS_MCP_TIMEOUT: %w", err)
		}
		cfg.Timeout = value
	}
	if raw := getenv("COSMOS_MCP_MAX_RESPONSE_BYTES"); raw != "" {
		value, err := parseBytes(raw)
		if err != nil {
			return Config{}, fmt.Errorf("COSMOS_MCP_MAX_RESPONSE_BYTES: %w", err)
		}
		cfg.MaxResponseBytes = value
	}

	fs := flag.NewFlagSet("cosmos-mcp", flag.ContinueOnError)
	var flagOutput strings.Builder
	fs.SetOutput(&flagOutput)
	fs.StringVar(&cfg.RPCURL, "rpc-url", cfg.RPCURL, "CometBFT JSON-RPC endpoint URL")
	fs.StringVar(&cfg.GRPCTarget, "grpc-target", cfg.GRPCTarget, "Cosmos gRPC target in host:port form")
	fs.BoolVar(&cfg.GRPCInsecure, "grpc-insecure", cfg.GRPCInsecure, "use plaintext gRPC instead of TLS")
	fs.StringVar(&cfg.LCDURL, "lcd-url", cfg.LCDURL, "Cosmos LCD/REST endpoint URL")
	fs.DurationVar(&cfg.Timeout, "timeout", cfg.Timeout, "upstream request timeout")
	maxBytes := byteSize(cfg.MaxResponseBytes)
	fs.Var(&maxBytes, "max-response-bytes", "maximum upstream response size (for example 4MiB)")
	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			fs.SetOutput(os.Stdout)
			fs.Usage()
			return Config{}, flag.ErrHelp
		}
		return Config{}, err
	}
	if fs.NArg() != 0 {
		return Config{}, fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " "))
	}
	cfg.RPCURL = strings.TrimSpace(cfg.RPCURL)
	cfg.GRPCTarget = strings.TrimSpace(cfg.GRPCTarget)
	cfg.LCDURL = strings.TrimSpace(cfg.LCDURL)
	cfg.MaxResponseBytes = int64(maxBytes)
	if err := cfg.Validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) Validate() error {
	if c.RPCURL == "" && c.GRPCTarget == "" && c.LCDURL == "" {
		return errors.New("at least one endpoint is required: --rpc-url, --grpc-target, or --lcd-url")
	}
	if c.Timeout <= 0 {
		return errors.New("timeout must be greater than zero")
	}
	if c.MaxResponseBytes <= 0 {
		return errors.New("max-response-bytes must be greater than zero")
	}
	for name, raw := range map[string]string{"rpc-url": c.RPCURL, "lcd-url": c.LCDURL} {
		if raw == "" {
			continue
		}
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (u.Scheme != "http" && u.Scheme != "https") {
			return fmt.Errorf("%s must be an absolute http(s) URL", name)
		}
		if u.Fragment != "" {
			return fmt.Errorf("%s must not contain a fragment", name)
		}
	}
	if c.GRPCTarget != "" {
		host, port, err := net.SplitHostPort(c.GRPCTarget)
		if err != nil || host == "" || port == "" {
			return errors.New("grpc-target must use host:port form")
		}
	} else if c.GRPCInsecure {
		return errors.New("grpc-insecure requires grpc-target")
	}
	return nil
}

type byteSize int64

func (b *byteSize) String() string { return strconv.FormatInt(int64(*b), 10) }

func (b *byteSize) Set(raw string) error {
	v, err := parseBytes(raw)
	if err == nil {
		*b = byteSize(v)
	}
	return err
}

func parseBytes(raw string) (int64, error) {
	s := strings.TrimSpace(raw)
	units := []struct {
		suffix string
		value  int64
	}{{"GiB", 1 << 30}, {"MiB", 1 << 20}, {"KiB", 1 << 10}, {"GB", 1_000_000_000}, {"MB", 1_000_000}, {"KB", 1_000}, {"B", 1}}
	multiplier := int64(1)
	for _, unit := range units {
		if strings.HasSuffix(strings.ToUpper(s), strings.ToUpper(unit.suffix)) {
			s = strings.TrimSpace(s[:len(s)-len(unit.suffix)])
			multiplier = unit.value
			break
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil || n <= 0 || n > (1<<63-1)/multiplier {
		return 0, fmt.Errorf("invalid byte size %q", raw)
	}
	return n * multiplier, nil
}
