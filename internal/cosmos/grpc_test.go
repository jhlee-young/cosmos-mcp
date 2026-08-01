package cosmos

import (
	"context"
	"encoding/json"
	"net"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/reflection"
	"google.golang.org/grpc/test/bufconn"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/reflect/protodesc"
	"google.golang.org/protobuf/reflect/protoreflect"
	"google.golang.org/protobuf/reflect/protoregistry"
	"google.golang.org/protobuf/types/descriptorpb"
	"google.golang.org/protobuf/types/known/emptypb"
	"google.golang.org/protobuf/types/known/structpb"
)

const testQueryFile = "cosmos_mcp_test_query.proto"

var registerTestDescriptor sync.Once

type testQueryServer interface {
	Echo(context.Context, *emptypb.Empty) (*structpb.Struct, error)
}

type testQueryImplementation struct{}

func (testQueryImplementation) Echo(context.Context, *emptypb.Empty) (*structpb.Struct, error) {
	return structpb.NewStruct(map[string]any{"ok": true})
}

func TestGRPCQueryWithReflection(t *testing.T) {
	client, stop := newTestGRPCClient(t, true)
	defer stop()
	files, descriptorErr := client.descriptorsForSymbol(context.Background(), "cosmos.mcp.test.v1.Query")
	if descriptorErr != nil {
		t.Fatalf("descriptorsForSymbol() error = %v", descriptorErr)
	}
	if _, descriptorErr = files.FindDescriptorByName("cosmos.mcp.test.v1.Query"); descriptorErr != nil {
		files.RangeFiles(func(file protoreflect.FileDescriptor) bool {
			t.Logf("reflected file: %s package=%s services=%d", file.Path(), file.Package(), file.Services().Len())
			return true
		})
		t.Fatalf("reflected service missing: %v", descriptorErr)
	}
	result, err := client.Query(context.Background(), "/cosmos.mcp.test.v1.Query/Echo", json.RawMessage(`{}`))
	if err != nil {
		t.Fatal(err)
	}
	if result.(map[string]any)["ok"] != true {
		t.Fatalf("Query() = %#v", result)
	}
	if _, err := client.Query(context.Background(), "/grpc.health.v1.Health/Check", json.RawMessage(`{}`)); errorCode(err) != CodeInvalidInput {
		t.Fatalf("non-query method error = %v", err)
	}
}

func TestGRPCQueryWithoutReflection(t *testing.T) {
	client, stop := newTestGRPCClient(t, false)
	defer stop()
	_, err := client.Query(context.Background(), "/cosmos.mcp.test.v1.Query/Echo", json.RawMessage(`{}`))
	if errorCode(err) != CodeGRPCReflectionUnavailable {
		t.Fatalf("Query() error = %v", err)
	}
}

func newTestGRPCClient(t *testing.T, withReflection bool) (*GRPCClient, func()) {
	t.Helper()
	var registerTestDescriptorErr error

	registerTestDescriptor.Do(func() {
		file, err := protodesc.NewFile(&descriptorpb.FileDescriptorProto{
			Name:       proto.String(testQueryFile),
			Package:    proto.String("cosmos.mcp.test.v1"),
			Syntax:     proto.String("proto3"),
			Dependency: []string{"google/protobuf/empty.proto", "google/protobuf/struct.proto"},
			Service: []*descriptorpb.ServiceDescriptorProto{{
				Name: proto.String("Query"),
				Method: []*descriptorpb.MethodDescriptorProto{{
					Name:       proto.String("Echo"),
					InputType:  proto.String(".google.protobuf.Empty"),
					OutputType: proto.String(".google.protobuf.Struct"),
				}},
			}},
		}, protoregistry.GlobalFiles)
		if err != nil {
			registerTestDescriptorErr = err
			return
		}
		if err := protoregistry.GlobalFiles.RegisterFile(file); err != nil {
			registerTestDescriptorErr = err
		}
	})
	if registerTestDescriptorErr != nil {
		t.Fatalf("register test descriptor: %v", registerTestDescriptorErr)
	}
	listener := bufconn.Listen(1 << 20)
	grpcServer := grpc.NewServer()
	grpcServer.RegisterService(&grpc.ServiceDesc{
		ServiceName: "cosmos.mcp.test.v1.Query",
		HandlerType: (*testQueryServer)(nil),
		Methods: []grpc.MethodDesc{{
			MethodName: "Echo",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(emptypb.Empty)
				if err := dec(in); err != nil {
					return nil, err
				}
				if interceptor == nil {
					return srv.(testQueryServer).Echo(ctx, in)
				}
				info := &grpc.UnaryServerInfo{Server: srv, FullMethod: "/cosmos.mcp.test.v1.Query/Echo"}
				handler := func(ctx context.Context, request any) (any, error) {
					return srv.(testQueryServer).Echo(ctx, request.(*emptypb.Empty))
				}
				return interceptor(ctx, in, info, handler)
			},
		}},
		Metadata: testQueryFile,
	}, testQueryImplementation{})
	if withReflection {
		reflection.Register(grpcServer)
	}
	go func() { _ = grpcServer.Serve(listener) }()
	conn, err := grpc.NewClient("passthrough:///bufnet",
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithContextDialer(func(context.Context, string) (net.Conn, error) { return listener.Dial() }),
	)
	if err != nil {
		t.Fatal(err)
	}
	client := &GRPCClient{target: "bufnet", timeout: time.Second, conn: conn, maxBytes: 1 << 20}
	return client, func() {
		_ = conn.Close()
		grpcServer.Stop()
		_ = listener.Close()
	}
}
