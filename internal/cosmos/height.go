package cosmos

import "context"

// HeightHeader is how the Cosmos SDK is asked to answer a query against
// historical state: gRPC reads it from request metadata, and the LCD's
// grpc-gateway forwards the HTTP header of the same name into that metadata.
// A node that has pruned the height rejects the query rather than silently
// answering from current state, so a successful response is always actually
// from the requested height.
const HeightHeader = "x-cosmos-block-height"

type heightContextKey struct{}

// WithHeight pins every LCD and gRPC read made with the returned context to a
// block height. It rides on the context rather than on Binding so that it
// applies to bindings added later without each one having to opt in.
func WithHeight(ctx context.Context, height string) context.Context {
	if height == "" {
		return ctx
	}
	return context.WithValue(ctx, heightContextKey{}, height)
}

func heightFrom(ctx context.Context) string {
	value, _ := ctx.Value(heightContextKey{}).(string)
	return value
}
