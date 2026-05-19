// Package wireerror is the v0.1 error contract: the typed, wavefront-originated
// failures returned to a client as a proper HTTP status + standard headers +
// the `X-Wavefront-Error` code + a fixed `wavefront.v1.Error` protobuf body.
// Established in v0.1 and stable through v1.0 (iterated additively). The code/
// status/header table here is authoritative against docs/protocol.md.
package wireerror

import "net/http"

const (
	codeUnsupportedContractVersion = "unsupported_contract_version"
	codeDecodeFailed               = "decode_failed"
	codeRequestBodyTooLarge        = "request_body_too_large"
	codeUpstreamTimeout            = "upstream_timeout"
	codeUpstreamError              = "upstream_error"
	codeTransformFailed            = "transform_failed"
)

// Error is a typed, wavefront-originated failure. It satisfies the error
// interface so it can flow through normal Go error handling.
type Error struct {
	code    string
	message string
	status  int
}

func (e *Error) Code() string    { return e.code }
func (e *Error) Message() string { return e.message }
func (e *Error) HTTPStatus() int { return e.status }
func (e *Error) Error() string   { return e.code + ": " + e.message }

// Headers is the standard + extension header set for this failure.
// Content-Type is always the protobuf media type; Retry-After is set only
// where retrying is semantically meaningful. The X-Wavefront-Error /
// X-Wavefront-Contract-Version headers are written by the server (the latter
// needs request context the error itself does not carry).
func (e *Error) Headers() http.Header {
	h := http.Header{}
	h.Set("Content-Type", "application/protobuf")
	if e.code == codeUpstreamTimeout {
		h.Set("Retry-After", "0")
	}
	return h
}

// ProtoBody is the marshaled wavefront.v1.Error response body.
func (e *Error) ProtoBody() []byte { return marshalError(e.code, e.message) }

func msgOr(msg, def string) string {
	if msg == "" {
		return def
	}
	return msg
}

// UnsupportedContractVersion — the contract-version header is missing,
// unknown, or unsupported. 400.
func UnsupportedContractVersion(msg string) *Error {
	return &Error{codeUnsupportedContractVersion, msgOr(msg, "unknown or missing contract version"), http.StatusBadRequest}
}

// DecodeFailed — the client body did not decode into the contract's
// request_message. 400.
func DecodeFailed(msg string) *Error {
	return &Error{codeDecodeFailed, msgOr(msg, "request body failed to decode"), http.StatusBadRequest}
}

// RequestBodyTooLarge — the inbound body exceeded WAVEFRONT_MAX_BODY_BYTES.
// 413.
func RequestBodyTooLarge(msg string) *Error {
	return &Error{codeRequestBodyTooLarge, msgOr(msg, "request body exceeds the configured limit"), http.StatusRequestEntityTooLarge}
}

// UpstreamTimeout — the upstream exceeded WAVEFRONT_REQUEST_TIMEOUT_MS. 504.
func UpstreamTimeout(msg string) *Error {
	return &Error{codeUpstreamTimeout, msgOr(msg, "upstream timed out"), http.StatusGatewayTimeout}
}

// UpstreamError — the upstream returned a non-2xx, was unreachable, or its
// reply could not be encoded. 502.
func UpstreamError(msg string) *Error {
	return &Error{codeUpstreamError, msgOr(msg, "upstream error"), http.StatusBadGateway}
}

// TransformFailedRequest — a request transform verb could not apply: the
// decoded request is well-formed but unprocessable under this contract's
// mapping. 422 (RFC 9110 §15.5.21).
func TransformFailedRequest(msg string) *Error {
	return &Error{codeTransformFailed, msgOr(msg, "request could not be transformed to the internal contract"), http.StatusUnprocessableEntity}
}

// TransformFailedResponse — a response transform verb could not apply: the
// live internal shape drifted from the bundle's response stanzas. Same fault
// class as upstream_error. 502.
func TransformFailedResponse(msg string) *Error {
	return &Error{codeTransformFailed, msgOr(msg, "upstream response could not be transformed to the client contract"), http.StatusBadGateway}
}
