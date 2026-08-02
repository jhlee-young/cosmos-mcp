package cosmos

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"
)

// negativeCacheTTL bounds how long a confirmed-unsupported capability is
// remembered before the resolver attempts its bindings again. This avoids a
// full round of reflection/route probes on every call for a capability the
// configured endpoints do not support, while still recovering automatically
// if the chain later adds support without a server restart.
const negativeCacheTTL = 5 * time.Minute

// defaultResolveBudget bounds the total wall-clock time a single Resolve
// call may spend trying its bindings in sequence. Without it, a capability
// with several gRPC bindings can cost up to two client timeouts per binding
// (one for reflection, one for the call), so the worst case is unbounded in
// the number of bindings attempted.
const defaultResolveBudget = 45 * time.Second

type Binding struct {
	Name               string
	GRPCMethod         string
	GRPCRequest        map[string]any
	GRPCDiscardUnknown bool
	LCDPath            string
	LCDQuery           map[string]string
	LCDQueryMulti      map[string][]string
	RPCMethod          string
	RPCParams          any
}

type Resolution struct {
	Data    any
	Source  string
	Binding string
}

type Resolver struct {
	RPC  *RPCClient
	LCD  *LCDClient
	GRPC *GRPCClient

	// Budget bounds the total time a single Resolve call may spend across
	// all of its binding attempts. Zero disables the bound.
	Budget time.Duration

	mu          sync.RWMutex
	cache       map[string]string
	unsupported map[string]unsupportedEntry
}

type unsupportedEntry struct {
	err     error
	expires time.Time
}

func NewResolver(rpc *RPCClient, lcd *LCDClient, grpc *GRPCClient) *Resolver {
	return &Resolver{RPC: rpc, LCD: lcd, GRPC: grpc, Budget: defaultResolveBudget, cache: make(map[string]string), unsupported: make(map[string]unsupportedEntry)}
}

func (r *Resolver) Resolve(ctx context.Context, capability string, bindings []Binding) (Resolution, error) {
	if err := r.cachedUnsupported(capability); err != nil {
		return Resolution{}, err
	}
	if r.Budget > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, r.Budget)
		defer cancel()
	}
	ordered := r.cachedFirst(capability, bindings)
	attempted := make([]string, 0, len(ordered))
	var discoveryErr error
	for _, binding := range ordered {
		if binding.Name == "" {
			binding.Name = bindingID(binding)
		}
		attempted = append(attempted, binding.Name)
		result, missing, err := r.invoke(ctx, binding)
		if missing {
			r.forget(capability, binding.Name)
			continue
		}
		if err == nil {
			r.mu.Lock()
			r.cache[capability] = binding.Name
			delete(r.unsupported, capability)
			r.mu.Unlock()
			return result, nil
		}
		if errorCodeOf(err) == CodeGRPCReflectionUnavailable {
			discoveryErr = err
			continue
		}
		return result, err
	}
	if discoveryErr != nil {
		return Resolution{}, discoveryErr
	}
	err := NewDetailedError(CodeUnsupportedCapability,
		fmt.Sprintf("configured endpoints do not support capability %q", capability), nil,
		map[string]any{"capability": capability, "attempted_bindings": attempted})
	r.mu.Lock()
	r.unsupported[capability] = unsupportedEntry{err: err, expires: time.Now().Add(negativeCacheTTL)}
	r.mu.Unlock()
	return Resolution{}, err
}

// cachedUnsupported returns the remembered error for a capability that every
// configured binding recently and definitively failed, without re-probing
// any endpoint. The entry expires after negativeCacheTTL so support added to
// the chain later is picked up without restarting the server.
func (r *Resolver) cachedUnsupported(capability string) error {
	r.mu.RLock()
	entry, ok := r.unsupported[capability]
	r.mu.RUnlock()
	if !ok {
		return nil
	}
	if time.Now().After(entry.expires) {
		r.mu.Lock()
		delete(r.unsupported, capability)
		r.mu.Unlock()
		return nil
	}
	return entry.err
}

func (r *Resolver) invoke(ctx context.Context, binding Binding) (Resolution, bool, error) {
	switch {
	case binding.GRPCMethod != "":
		if r.GRPC == nil {
			return Resolution{}, true, nil
		}
		ok, err := r.GRPC.HasMethod(ctx, binding.GRPCMethod)
		if err != nil || !ok {
			return Resolution{}, !ok && err == nil, err
		}
		request, err := encodeJSON(binding.GRPCRequest)
		if err != nil {
			return Resolution{}, false, NewError(CodeInvalidInput, "gRPC request could not be encoded", err)
		}
		var data any
		if binding.GRPCDiscardUnknown {
			data, err = r.GRPC.QueryDiscardUnknown(ctx, binding.GRPCMethod, request)
		} else {
			data, err = r.GRPC.Query(ctx, binding.GRPCMethod, request)
		}
		if err != nil && IsGRPCMethodUnavailable(err) {
			return Resolution{}, true, err
		}
		return Resolution{Data: data, Source: "grpc", Binding: binding.GRPCMethod}, false, err
	case binding.LCDPath != "":
		if r.LCD == nil {
			return Resolution{}, true, nil
		}
		var data any
		var err error
		if binding.LCDQueryMulti != nil {
			data, err = r.LCD.GetMulti(ctx, binding.LCDPath, binding.LCDQueryMulti)
		} else {
			data, err = r.LCD.Get(ctx, binding.LCDPath, binding.LCDQuery)
		}
		if err != nil && IsRouteNotFound(err) {
			return Resolution{}, true, err
		}
		return Resolution{Data: data, Source: "lcd", Binding: binding.LCDPath}, false, err
	case binding.RPCMethod != "":
		if r.RPC == nil {
			return Resolution{}, true, nil
		}
		data, err := r.RPC.Call(ctx, binding.RPCMethod, binding.RPCParams)
		return Resolution{Data: data, Source: "rpc", Binding: binding.RPCMethod}, false, err
	default:
		return Resolution{}, true, nil
	}
}

func (r *Resolver) cachedFirst(capability string, bindings []Binding) []Binding {
	r.mu.RLock()
	cached := r.cache[capability]
	r.mu.RUnlock()
	if cached == "" {
		return bindings
	}
	result := make([]Binding, 0, len(bindings))
	for _, item := range bindings {
		name := item.Name
		if name == "" {
			name = bindingID(item)
		}
		if name == cached {
			result = append(result, item)
		}
	}
	for _, item := range bindings {
		name := item.Name
		if name == "" {
			name = bindingID(item)
		}
		if name != cached {
			result = append(result, item)
		}
	}
	return result
}

func (r *Resolver) forget(capability, binding string) {
	r.mu.Lock()
	if r.cache[capability] == binding {
		delete(r.cache, capability)
	}
	r.mu.Unlock()
}

func bindingID(binding Binding) string {
	switch {
	case binding.GRPCMethod != "":
		return binding.GRPCMethod
	case binding.LCDPath != "":
		return binding.LCDPath
	default:
		return binding.RPCMethod
	}
}

func encodeJSON(value any) (json.RawMessage, error) {
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	return encoded, nil
}
