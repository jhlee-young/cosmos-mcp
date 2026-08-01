package cosmos

import (
	"errors"
	"fmt"
)

type ErrorCode string

const (
	CodeInvalidInput              ErrorCode = "invalid_input"
	CodeNotConfigured             ErrorCode = "not_configured"
	CodeEndpointUnavailable       ErrorCode = "endpoint_unavailable"
	CodeUpstreamTimeout           ErrorCode = "upstream_timeout"
	CodeUpstreamError             ErrorCode = "upstream_error"
	CodeResponseTooLarge          ErrorCode = "response_too_large"
	CodeGRPCReflectionUnavailable ErrorCode = "grpc_reflection_unavailable"
)

type Error struct {
	Code    ErrorCode
	Message string
	Err     error
}

func (e *Error) Error() string {
	if e.Message != "" {
		return e.Message
	}
	return string(e.Code)
}

func (e *Error) Unwrap() error { return e.Err }

func NewError(code ErrorCode, message string, err error) error {
	return &Error{Code: code, Message: message, Err: err}
}

func ErrorDetails(err error) (ErrorCode, string) {
	var target *Error
	if errors.As(err, &target) {
		return target.Code, target.Error()
	}
	return CodeUpstreamError, fmt.Sprintf("upstream request failed: %v", err)
}
