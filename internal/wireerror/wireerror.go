// Package wireerror is the error contract: the typed, wavefront-originated
// failures returned to a client as a proper HTTP status + standard headers +
// the `X-Wavefront-Error` code + a fixed `wavefront.v0.Error` protobuf body.
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
	codeInternalError              = "internal_error"
	codeUnknownRoute               = "unknown_route"
	codeUpstreamStatus             = "upstream_status"
	codeUnsupportedMediaType       = "unsupported_media_type"
	codeUnavailable                = "unavailable"
)

// MediaTypeProtobuf is the single request and response media type the v0.1
// codec contract names. Every wire-error envelope has Content-Type set to
// this value; an inbound request that carries a body must declare it. A
// future multi-codec selector (issue #44) would replace direct comparison
// against this constant with a per-codec table.
const MediaTypeProtobuf = "application/protobuf"

// Error is a typed, wavefront-originated failure. It satisfies the error
// interface so it can flow through normal Go error handling.
//
// `status` is per-instance rather than per-code because `upstream_status`
// parameterizes the HTTP status across any undeclared non-success status
// outside the capability ceiling. Every other code in the table has a
// fixed status — passed at construction and never varied.
//
// `retryAfter` is the verbatim header value to emit. It is empty for every
// code except `upstream_timeout` (which fixes it to "0"), `upstream_status`
// on a 429 whose upstream supplied a Retry-After header, and `unavailable`
// emitted by the proxy bundle-gate (which attaches a short fixed hint so
// clients back off briefly during boot).
type Error struct {
	code       string
	message    string
	status     int
	retryAfter string
}

func (e *Error) Code() string    { return e.code }
func (e *Error) Message() string { return e.message }
func (e *Error) HTTPStatus() int { return e.status }
func (e *Error) Error() string   { return e.code + ": " + e.message }

// WithRetryAfter returns a shallow copy of e with the given Retry-After
// header value attached. An empty string is a no-op so callers can pass
// `uresp.Header.Get("Retry-After")` directly without branching on presence.
// Used by the upstream 429 passthrough and by the proxy bundle-gate's
// `unavailable` emission; other codes either set Retry-After at
// construction (upstream_timeout's fixed "0") or never emit it.
func (e *Error) WithRetryAfter(v string) *Error {
	if v == "" {
		return e
	}
	c := *e
	c.retryAfter = v
	return &c
}

// Headers is the standard + extension header set for this failure.
// Content-Type is always the protobuf media type; Retry-After is set only
// where retrying is semantically meaningful. The X-Wavefront-Error /
// X-Wavefront-Contract-Version headers are written by the server (the latter
// needs request context the error itself does not carry).
func (e *Error) Headers() http.Header {
	h := http.Header{}
	h.Set("Content-Type", MediaTypeProtobuf)
	switch {
	case e.code == codeUpstreamTimeout:
		h.Set("Retry-After", "0")
	case e.retryAfter != "":
		h.Set("Retry-After", e.retryAfter)
	}
	return h
}

// ProtoBody is the marshaled wavefront.v0.Error response body.
func (e *Error) ProtoBody() []byte { return marshalError(e.code, e.message) }

// Wavefront response header names that the wire-error envelope owns —
// kept here, alongside the codes, so the server and any other emitter
// cannot drift from the protocol definition.
const (
	HeaderContentType     = "Content-Type"
	HeaderContractVersion = "X-Wavefront-Contract-Version"
	HeaderWavefrontError  = "X-Wavefront-Error"
)

// VersionUnknown is the X-Wavefront-Contract-Version value to send when
// negotiation has not resolved a real version yet — a pre-negotiate error,
// a panic before the bundle is consulted, or any other path that lacks
// request context. It keeps the header well-defined on every response,
// success or error, as docs/protocol.md requires.
const VersionUnknown = "unknown"

// Write emits a wavefront-originated failure as a fully-formed wire-error
// envelope: status, standard + extension headers, and the protobuf body.
// It is the single path every emitter calls — direct callers in handlers,
// the panic-recovery middleware, and any future emitter must go through
// this function so the on-wire shape cannot drift.
//
// contractVersion is the value sent in X-Wavefront-Contract-Version; pass
// VersionUnknown when no negotiation has resolved a real version yet.
func Write(w http.ResponseWriter, werr *Error, contractVersion string) {
	if contractVersion == "" {
		contractVersion = VersionUnknown
	}
	// Set (not Add) for every header: the wire-error contract has no
	// list-valued headers, and Add would produce duplicate values if the
	// caller had pre-populated the response header or if Write were ever
	// called twice on the same response.
	for k, vs := range werr.Headers() {
		for _, v := range vs {
			w.Header().Set(k, v)
		}
	}
	w.Header().Set(HeaderWavefrontError, werr.Code())
	w.Header().Set(HeaderContractVersion, contractVersion)
	w.WriteHeader(werr.HTTPStatus())
	_, _ = w.Write(werr.ProtoBody())
}

func msgOr(msg, def string) string {
	if msg == "" {
		return def
	}
	return msg
}

// UnsupportedContractVersion — the contract-version header is missing,
// unknown, or unsupported. 400.
func UnsupportedContractVersion(msg string) *Error {
	return &Error{
		code:    codeUnsupportedContractVersion,
		message: msgOr(msg, "unknown or missing contract version"),
		status:  http.StatusBadRequest,
	}
}

// DecodeFailed — the client body did not decode into the contract's
// request_message. 400.
func DecodeFailed(msg string) *Error {
	return &Error{
		code:    codeDecodeFailed,
		message: msgOr(msg, "request body failed to decode"),
		status:  http.StatusBadRequest,
	}
}

// RequestBodyTooLarge — the inbound body exceeded WAVEFRONT_MAX_BODY_BYTES.
// 413.
func RequestBodyTooLarge(msg string) *Error {
	return &Error{
		code:    codeRequestBodyTooLarge,
		message: msgOr(msg, "request body exceeds the configured limit"),
		status:  http.StatusRequestEntityTooLarge,
	}
}

// UpstreamTimeout — the upstream exceeded WAVEFRONT_REQUEST_TIMEOUT_MS. 504.
func UpstreamTimeout(msg string) *Error {
	return &Error{
		code:    codeUpstreamTimeout,
		message: msgOr(msg, "upstream timed out"),
		status:  http.StatusGatewayTimeout,
	}
}

// UpstreamError — the upstream returned a non-2xx, was unreachable, or its
// reply could not be encoded. 502.
func UpstreamError(msg string) *Error {
	return &Error{
		code:    codeUpstreamError,
		message: msgOr(msg, "upstream error"),
		status:  http.StatusBadGateway,
	}
}

// TransformFailedRequest — a request transform verb could not apply: the
// decoded request is well-formed but unprocessable under this contract's
// mapping. 422 (RFC 9110 §15.5.21).
func TransformFailedRequest(msg string) *Error {
	return &Error{
		code:    codeTransformFailed,
		message: msgOr(msg, "request could not be transformed to the internal contract"),
		status:  http.StatusUnprocessableEntity,
	}
}

// TransformFailedResponse — a response transform verb could not apply: the
// live internal shape drifted from the bundle's response stanzas. Same fault
// class as upstream_error. 502.
func TransformFailedResponse(msg string) *Error {
	return &Error{
		code:    codeTransformFailed,
		message: msgOr(msg, "upstream response could not be transformed to the client contract"),
		status:  http.StatusBadGateway,
	}
}

// InternalError — an unrecoverable fault inside wavefront itself: a panic in
// the request path, a programmer error caught at runtime, anything that is
// not a client / upstream condition. 500. The detail message should not leak
// internals to clients; callers pass a short, fixed string and rely on logs
// for the panic value and stack trace.
func InternalError(msg string) *Error {
	return &Error{
		code:    codeInternalError,
		message: msgOr(msg, "internal server error"),
		status:  http.StatusInternalServerError,
	}
}

// UnknownRoute — no contract in the bundle binds the inbound request's
// (path, method). Wrong-method folds into this code: a request whose path
// matches a registered route but whose method differs returns the same
// envelope (no 405, no `Allow` header), because each contract names exactly
// one method and the bundle is the only routing source of truth. 404.
func UnknownRoute(msg string) *Error {
	return &Error{
		code:    codeUnknownRoute,
		message: msgOr(msg, "no contract binds this request's path and method"),
		status:  http.StatusNotFound,
	}
}

// UnsupportedMediaType — the inbound request declared a Content-Type that is
// not the contract's protobuf media type. Distinct from `decode_failed`: a
// wrong envelope is rejected before the body is read, so this code is
// reserved for the envelope mismatch and `decode_failed` is reserved for a
// valid envelope whose bytes don't parse. 415 (RFC 9110 §15.5.16).
func UnsupportedMediaType(msg string) *Error {
	return &Error{
		code:    codeUnsupportedMediaType,
		message: msgOr(msg, "request Content-Type is not "+MediaTypeProtobuf),
		status:  http.StatusUnsupportedMediaType,
	}
}

// Unavailable — the proxy cannot serve right now because the bundle is not
// loaded yet. 503. Issue #39's matrix extends this code — used on the
// /ready ops endpoint since v0.1 — to the proxy data path: a request that
// races bundle-load at boot returns this envelope rather than the
// misleading `upstream_error` 502 it used to (there is no upstream
// involved). The caller is expected to attach a short Retry-After hint
// with `.WithRetryAfter(...)` so well-behaved clients back off briefly and
// retry. Graceful drain is handled implicitly by http.Server.Shutdown
// (the bundle pointer is never cleared at shutdown), so accepted requests
// in the drain window still find a live bundle and finish normally.
func Unavailable(msg string) *Error {
	return &Error{
		code:    codeUnavailable,
		message: msgOr(msg, "service unavailable"),
		status:  http.StatusServiceUnavailable,
	}
}

// IsCapabilityCeiling reports whether status names an HTTP feature wavefront
// does not model and never will at the proxy edge: 206 Partial Content (no
// Range support), 207/208 WebDAV, 226 IM Used (delta encoding), and every 3xx
// redirect. Such a status is a hard 502 regardless of any declared binding or
// per-route strictness — it is a capability statement, not a policy choice.
func IsCapabilityCeiling(status int) bool {
	switch status {
	case 206, 207, 208, 226:
		return true
	}
	return status >= 300 && status <= 399
}

// UpstreamStatus — the upstream returned an undeclared non-success status
// outside the capability ceiling, on a non-strict route. The upstream's status
// is preserved on the response (so a 503 stays a 503); the body is a
// wavefront.v0.Error aid envelope whose message field relays the upstream body
// (UTF-8-coerced), so the wire-error body type holds while the client still
// sees the upstream's own detail. This is the only wire-error code whose HTTP
// status is parameterized — every other constructor pins a fixed status.
//
// For an upstream 429, the caller can attach the upstream's Retry-After value
// with `.WithRetryAfter(...)`; the header is relayed verbatim. For any other
// status, Retry-After is not emitted.
func UpstreamStatus(httpStatus int, msg string) *Error {
	return &Error{
		code:    codeUpstreamStatus,
		message: msgOr(msg, "upstream returned "+http.StatusText(httpStatus)),
		status:  httpStatus,
	}
}
