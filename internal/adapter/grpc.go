package adapter

import (
	"encoding/binary"
	"math"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

const (
	// MediaTypeGRPCProto identifies protobuf messages carried by gRPC.
	MediaTypeGRPCProto = "application/grpc+proto"

	// A gRPC message envelope is one compression byte followed by a four-byte
	// unsigned message length in network byte order.
	grpcEnvelopeSize = 5
)

// GRPCJSON is the unary gRPC variant of the protobuf-to-JSON adapter. It owns
// only message framing and codec conversion: an HTTP/2 transport selects it,
// routes the RPC to a Binding, and writes grpc-status trailers.
//
// Unary means exactly one uncompressed request message and one response
// message. Compression and streaming require negotiation and lifecycle state
// outside this adapter and are deliberately rejected here.
type GRPCJSON struct {
	protoJSON *ProtoJSON
}

func NewGRPCJSON(r MessageResolver) *GRPCJSON {
	return &GRPCJSON{protoJSON: NewProtoJSON(r)}
}

var _ Adapter = (*GRPCJSON)(nil)

func (g *GRPCJSON) DecodeRequest(b Binding, in []byte) (UpstreamCall, *wireerror.Error) {
	payload, werr := decodeUnaryGRPC(in)
	if werr != nil {
		return UpstreamCall{}, werr
	}
	return g.protoJSON.DecodeRequest(b, payload)
}

func (g *GRPCJSON) EncodeResponse(b Binding, upstreamJSON []byte) ([]byte, string, *wireerror.Error) {
	payload, _, werr := g.protoJSON.EncodeResponse(b, upstreamJSON)
	if werr != nil {
		return nil, "", werr
	}
	out, werr := encodeUnaryGRPC(payload)
	if werr != nil {
		return nil, "", werr
	}
	return out, MediaTypeGRPCProto, nil
}

// EncodeError converts a declared upstream error body using its bound message
// and frames it. The HTTP/2 transport remains responsible for mapping the
// upstream status to grpc-status and deciding whether that framed detail is
// sent to the client.
func (g *GRPCJSON) EncodeError(b Binding, status int, upstreamJSON []byte) ([]byte, string, *wireerror.Error) {
	payload, _, werr := g.protoJSON.EncodeError(b, status, upstreamJSON)
	if werr != nil {
		return nil, "", werr
	}
	out, werr := encodeUnaryGRPC(payload)
	if werr != nil {
		return nil, "", werr
	}
	return out, MediaTypeGRPCProto, nil
}

// decodeUnaryGRPC validates and removes one gRPC message envelope. Requiring
// the declared length to equal the remaining body length rejects truncation,
// trailing bytes, and a second frame with the same bounded check.
func decodeUnaryGRPC(in []byte) ([]byte, *wireerror.Error) {
	if len(in) < grpcEnvelopeSize {
		return nil, wireerror.DecodeFailed("request body is not a complete unary gRPC envelope")
	}
	if in[0] != 0 {
		if in[0] == 1 {
			return nil, wireerror.DecodeFailed("compressed unary gRPC messages are not supported")
		}
		return nil, wireerror.DecodeFailed("request body has an invalid gRPC compression flag")
	}

	declared := uint64(binary.BigEndian.Uint32(in[1:grpcEnvelopeSize]))
	actual := uint64(len(in) - grpcEnvelopeSize)
	if declared > actual {
		return nil, wireerror.DecodeFailed("unary gRPC message is truncated")
	}
	if declared < actual {
		return nil, wireerror.DecodeFailed("unary gRPC request must contain exactly one message")
	}
	return in[grpcEnvelopeSize:], nil
}

func encodeUnaryGRPC(payload []byte) ([]byte, *wireerror.Error) {
	if uint64(len(payload)) > math.MaxUint32 {
		return nil, wireerror.UpstreamError("upstream response exceeds the gRPC message length limit")
	}
	out := make([]byte, grpcEnvelopeSize+len(payload))
	binary.BigEndian.PutUint32(out[1:grpcEnvelopeSize], uint32(len(payload)))
	copy(out[grpcEnvelopeSize:], payload)
	return out, nil
}
