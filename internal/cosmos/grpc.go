package cosmos

//lint:file-ignore SA1019 Legacy Cosmos nodes may expose only the deprecated gRPC reflection v1alpha API.

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	grpc_health_v1 "google.golang.org/grpc/health/grpc_health_v1"
	reflectionv1 "google.golang.org/grpc/reflection/grpc_reflection_v1"
	reflectionv1alpha "google.golang.org/grpc/reflection/grpc_reflection_v1alpha"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/dynamicpb"
)

// symbolNotFoundTTL bounds how long a gRPC service confirmed absent by
// reflection is remembered, so repeated queries for an unsupported module
// don't pay for a reflection round trip on every call.
const symbolNotFoundTTL = 5 * time.Minute

type GRPCClient struct {
	target      string
	timeout     time.Duration
	conn        *grpc.ClientConn
	maxBytes    int64
	mu          sync.RWMutex
	descriptors map[string]*protoregistry.Files
	missing     map[string]time.Time
}

func NewGRPCClient(target string, plaintext bool, timeout time.Duration, maxBytes int64) (*GRPCClient, error) {
	if timeout <= 0 {
		timeout = defaultTimeout
	}
	if maxBytes <= 0 {
		maxBytes = defaultMaxBytes
	}
	if maxBytes > math.MaxInt32 {
		maxBytes = math.MaxInt32
	}
	var transport credentials.TransportCredentials
	if plaintext {
		transport = insecure.NewCredentials()
	} else {
		transport = credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12})
	}
	conn, err := grpc.NewClient(target,
		grpc.WithTransportCredentials(transport),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(int(maxBytes))),
	)
	if err != nil {
		return nil, err
	}
	return &GRPCClient{target: target, timeout: timeout, conn: conn, maxBytes: maxBytes, descriptors: make(map[string]*protoregistry.Files), missing: make(map[string]time.Time)}, nil
}

func (c *GRPCClient) Endpoint() string { return c.target }
func (c *GRPCClient) Close() error     { return c.conn.Close() }

func (c *GRPCClient) Status(ctx context.Context) error {
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	_, err := grpc_health_v1.NewHealthClient(c.conn).Check(ctx, &grpc_health_v1.HealthCheckRequest{})
	if err == nil {
		return nil
	}
	if status.Code(err) != codes.Unimplemented {
		return mapGRPCError(err)
	}
	if err := c.listServices(ctx); err != nil {
		return err
	}
	return nil
}

func (c *GRPCClient) Query(ctx context.Context, fullMethod string, input json.RawMessage) (any, error) {
	return c.query(ctx, fullMethod, input, false)
}

// QueryDiscardUnknown invokes a reflected method while projecting the JSON
// request onto the server's input schema. It is intended for internal
// compatibility bindings that carry fields from multiple API versions.
func (c *GRPCClient) QueryDiscardUnknown(ctx context.Context, fullMethod string, input json.RawMessage) (any, error) {
	return c.query(ctx, fullMethod, input, true)
}

func (c *GRPCClient) query(ctx context.Context, fullMethod string, input json.RawMessage, discardUnknown bool) (any, error) {
	fullMethod, serviceName, methodName, err := normalizeGRPCMethod(fullMethod)
	if err != nil {
		return nil, err
	}
	if !allowedGRPCMethod(serviceName, methodName) {
		return nil, NewError(CodeInvalidInput, "gRPC method is not allowed in read-only mode", nil)
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	files, err := c.descriptorsForSymbol(ctx, serviceName)
	if err != nil {
		return nil, err
	}
	descriptor, err := files.FindDescriptorByName(protoreflect.FullName(serviceName))
	if err != nil {
		return nil, NewError(CodeUpstreamError, "gRPC reflection did not return the requested service", err)
	}
	service, ok := descriptor.(protoreflect.ServiceDescriptor)
	if !ok {
		return nil, NewError(CodeUpstreamError, "reflected symbol is not a gRPC service", nil)
	}
	method := service.Methods().ByName(protoreflect.Name(methodName))
	if method == nil {
		return nil, NewError(CodeInvalidInput, "gRPC method was not found in the reflected service", nil)
	}
	if method.IsStreamingClient() || method.IsStreamingServer() {
		return nil, NewError(CodeInvalidInput, "streaming gRPC methods are not supported", nil)
	}
	request := dynamicpb.NewMessage(method.Input())
	if len(input) == 0 {
		input = json.RawMessage(`{}`)
	}
	if err := (protojson.UnmarshalOptions{DiscardUnknown: discardUnknown}).Unmarshal(input, request); err != nil {
		return nil, NewError(CodeInvalidInput, "gRPC request does not match the reflected input schema", err)
	}
	response := dynamicpb.NewMessage(method.Output())
	if err := c.conn.Invoke(ctx, fullMethod, request, response); err != nil {
		return nil, mapGRPCError(err)
	}
	encoded, err := (protojson.MarshalOptions{UseProtoNames: true}).Marshal(response)
	if err != nil {
		return nil, NewError(CodeUpstreamError, "failed to encode gRPC response", err)
	}
	if int64(len(encoded)) > c.maxBytes {
		return nil, NewError(CodeResponseTooLarge, "upstream response exceeded the configured size limit", nil)
	}
	var result any
	if err := json.Unmarshal(encoded, &result); err != nil {
		return nil, NewError(CodeUpstreamError, "failed to convert gRPC response to JSON", err)
	}
	return result, nil
}

func (c *GRPCClient) HasMethod(ctx context.Context, fullMethod string) (bool, error) {
	_, serviceName, methodName, err := normalizeGRPCMethod(fullMethod)
	if err != nil {
		return false, err
	}
	ctx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	files, err := c.descriptorsForSymbol(ctx, serviceName)
	if err != nil {
		if errorCodeOf(err) == CodeInvalidInput {
			return false, nil
		}
		return false, err
	}
	descriptor, err := files.FindDescriptorByName(protoreflect.FullName(serviceName))
	if err != nil {
		return false, nil
	}
	service, ok := descriptor.(protoreflect.ServiceDescriptor)
	return ok && service.Methods().ByName(protoreflect.Name(methodName)) != nil, nil
}

func normalizeGRPCMethod(raw string) (full, service, method string, err error) {
	raw = strings.TrimSpace(raw)
	if !strings.HasPrefix(raw, "/") {
		raw = "/" + raw
	}
	parts := strings.Split(strings.TrimPrefix(raw, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || !strings.Contains(parts[0], ".") {
		return "", "", "", NewError(CodeInvalidInput, "gRPC method must use /package.Service/Method form", nil)
	}
	return raw, parts[0], parts[1], nil
}

// allowedGRPCMethod trusts the Cosmos SDK convention that any service named
// "...Query" is read-only; this is not enforced by the gRPC protocol itself,
// so a chain that violates the convention could expose a mutating method here.
func allowedGRPCMethod(service, method string) bool {
	if strings.HasSuffix(service, ".Query") {
		return true
	}
	allowed := map[string]map[string]bool{
		"cosmos.base.tendermint.v1beta1.Service": {
			"GetNodeInfo": true, "GetSyncing": true, "GetLatestBlock": true, "GetBlockByHeight": true,
			"GetLatestValidatorSet": true, "GetValidatorSetByHeight": true,
		},
		"cosmos.base.node.v1beta1.Service": {"Config": true, "Status": true},
		"cosmos.tx.v1beta1.Service":        {"GetTx": true, "GetTxsEvent": true, "Simulate": true},
	}
	return allowed[service][method]
}

func (c *GRPCClient) listServices(ctx context.Context) error { //nolint:staticcheck // v1alpha fallback supports legacy Cosmos nodes.
	stream, err := reflectionv1.NewServerReflectionClient(c.conn).ServerReflectionInfo(ctx)
	if err == nil {
		err = stream.Send(&reflectionv1.ServerReflectionRequest{MessageRequest: &reflectionv1.ServerReflectionRequest_ListServices{ListServices: ""}})
		if err == nil {
			_, err = stream.Recv()
		}
	}
	if err == nil {
		return nil
	}
	streamAlpha, alphaErr := reflectionv1alpha.NewServerReflectionClient(c.conn).ServerReflectionInfo(ctx)
	if alphaErr == nil {
		alphaErr = streamAlpha.Send(&reflectionv1alpha.ServerReflectionRequest{MessageRequest: &reflectionv1alpha.ServerReflectionRequest_ListServices{ListServices: ""}}) //nolint:staticcheck // Legacy reflection fallback.
		if alphaErr == nil {
			_, alphaErr = streamAlpha.Recv()
		}
	}
	if alphaErr != nil {
		return NewError(CodeGRPCReflectionUnavailable, "gRPC health and reflection services are unavailable", alphaErr)
	}
	return nil
}

func (c *GRPCClient) descriptorsForSymbol(ctx context.Context, symbol string) (*protoregistry.Files, error) {
	c.mu.RLock()
	cached := c.descriptors[symbol]
	missingUntil, isMissing := c.missing[symbol]
	c.mu.RUnlock()
	if cached != nil {
		return cached, nil
	}
	if isMissing && time.Now().Before(missingUntil) {
		return nil, NewError(CodeInvalidInput, "gRPC service was not found by reflection", nil)
	}
	raw, err := c.fetchSymbol(ctx, symbol)
	if err != nil {
		if errorCodeOf(err) == CodeInvalidInput {
			c.mu.Lock()
			if c.missing == nil {
				c.missing = make(map[string]time.Time)
			}
			c.missing[symbol] = time.Now().Add(symbolNotFoundTTL)
			c.mu.Unlock()
		}
		return nil, err
	}
	protos := make(map[string]*descriptorpb.FileDescriptorProto)
	for _, item := range raw {
		fd := new(descriptorpb.FileDescriptorProto)
		if err := proto.Unmarshal(item, fd); err != nil {
			return nil, NewError(CodeUpstreamError, "gRPC reflection returned an invalid descriptor", err)
		}
		protos[fd.GetName()] = fd
	}
	registry := new(protoregistry.Files)
	building := make(map[string]bool)
	var build func(string) error
	build = func(name string) error {
		if _, err := registry.FindFileByPath(name); err == nil {
			return nil
		}
		if globalFile, err := protoregistry.GlobalFiles.FindFileByPath(name); err == nil {
			if protos[name] != nil {
				return registry.RegisterFile(globalFile)
			}
			return nil
		}
		fd := protos[name]
		if fd == nil {
			return fmt.Errorf("missing dependency %s", name)
		}
		if building[name] {
			return fmt.Errorf("descriptor dependency cycle at %s", name)
		}
		building[name] = true
		defer delete(building, name)
		for _, dependency := range fd.Dependency {
			if err := build(dependency); err != nil {
				return err
			}
		}
		file, err := protodesc.NewFile(fd, combinedResolver{local: registry})
		if err != nil {
			return err
		}
		return registry.RegisterFile(file)
	}
	for name := range protos {
		if err := build(name); err != nil {
			return nil, NewError(CodeUpstreamError, "could not resolve reflected gRPC descriptors", err)
		}
	}
	c.mu.Lock()
	if c.descriptors == nil {
		c.descriptors = make(map[string]*protoregistry.Files)
	}
	if existing := c.descriptors[symbol]; existing != nil {
		registry = existing
	} else {
		c.descriptors[symbol] = registry
	}
	delete(c.missing, symbol)
	c.mu.Unlock()
	return registry, nil
}

func IsGRPCMethodUnavailable(err error) bool {
	return status.Code(err) == codes.Unimplemented
}

func errorCodeOf(err error) ErrorCode {
	code, _ := ErrorDetails(err)
	return code
}

func (c *GRPCClient) fetchSymbol(ctx context.Context, symbol string) ([][]byte, error) { //nolint:staticcheck // v1alpha fallback supports legacy Cosmos nodes.
	stream, err := reflectionv1.NewServerReflectionClient(c.conn).ServerReflectionInfo(ctx)
	if err == nil {
		err = stream.Send(&reflectionv1.ServerReflectionRequest{MessageRequest: &reflectionv1.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol}})
		if err == nil {
			var response *reflectionv1.ServerReflectionResponse
			response, err = stream.Recv()
			if err == nil && response.GetFileDescriptorResponse() != nil {
				return response.GetFileDescriptorResponse().FileDescriptorProto, nil
			}
			if err == nil && response.GetErrorResponse() != nil {
				err = status.Error(codes.Code(response.GetErrorResponse().GetErrorCode()), response.GetErrorResponse().GetErrorMessage())
			}
		}
	}
	streamAlpha, alphaErr := reflectionv1alpha.NewServerReflectionClient(c.conn).ServerReflectionInfo(ctx)
	if alphaErr == nil {
		alphaErr = streamAlpha.Send(&reflectionv1alpha.ServerReflectionRequest{MessageRequest: &reflectionv1alpha.ServerReflectionRequest_FileContainingSymbol{FileContainingSymbol: symbol}}) //nolint:staticcheck // Legacy reflection fallback.
		if alphaErr == nil {
			var response *reflectionv1alpha.ServerReflectionResponse //nolint:staticcheck // Legacy reflection fallback.
			response, alphaErr = streamAlpha.Recv()
			if alphaErr == nil && response.GetFileDescriptorResponse() != nil { //nolint:staticcheck // Legacy reflection fallback.
				return response.GetFileDescriptorResponse().FileDescriptorProto, nil //nolint:staticcheck // Legacy reflection fallback.
			}
			if alphaErr == nil && response.GetErrorResponse() != nil { //nolint:staticcheck // Legacy reflection fallback.
				alphaErr = status.Error(codes.Code(response.GetErrorResponse().GetErrorCode()), response.GetErrorResponse().GetErrorMessage()) //nolint:staticcheck // Legacy reflection fallback.
			}
		}
	}
	if status.Code(err) == codes.NotFound || status.Code(alphaErr) == codes.NotFound {
		return nil, NewError(CodeInvalidInput, "gRPC service was not found by reflection", nil)
	}
	return nil, NewError(CodeGRPCReflectionUnavailable, "gRPC server reflection is unavailable; use LCD or enable reflection on the node", errors.Join(err, alphaErr))
}

type combinedResolver struct{ local *protoregistry.Files }

func (r combinedResolver) FindFileByPath(path string) (protoreflect.FileDescriptor, error) {
	if file, err := r.local.FindFileByPath(path); err == nil {
		return file, nil
	}
	return protoregistry.GlobalFiles.FindFileByPath(path)
}

func (r combinedResolver) FindDescriptorByName(name protoreflect.FullName) (protoreflect.Descriptor, error) {
	if descriptor, err := r.local.FindDescriptorByName(name); err == nil {
		return descriptor, nil
	}
	return protoregistry.GlobalFiles.FindDescriptorByName(name)
}

func mapGRPCError(err error) error {
	switch status.Code(err) {
	case codes.DeadlineExceeded:
		return NewError(CodeUpstreamTimeout, "gRPC request timed out", err)
	case codes.Unavailable, codes.Canceled:
		return NewError(CodeEndpointUnavailable, "gRPC endpoint is unavailable", err)
	case codes.ResourceExhausted:
		return NewError(CodeResponseTooLarge, "gRPC response exceeded the configured size limit", err)
	case codes.InvalidArgument, codes.NotFound:
		return NewError(CodeInvalidInput, status.Convert(err).Message(), err)
	default:
		return NewError(CodeUpstreamError, fmt.Sprintf("gRPC request failed: %s", status.Convert(err).Message()), err)
	}
}
