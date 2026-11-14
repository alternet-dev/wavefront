package server

import (
	"math"
	"mime"
	"strconv"
	"strings"

	"github.com/alternet-dev/wavefront/internal/adapter"
)

// codecRegistry owns the server's immutable media-type-to-adapter mapping.
// Order is the server preference used only when Accept gives equally weighted
// choices and there is no request codec to break the tie.
type codecRegistry struct {
	byMediaType map[string]adapter.Adapter
	preference  []string
}

func newCodecRegistry(adapters ...adapter.Adapter) codecRegistry {
	r := codecRegistry{byMediaType: make(map[string]adapter.Adapter, len(adapters))}
	for _, a := range adapters {
		mediaType := strings.ToLower(a.MediaType())
		if _, exists := r.byMediaType[mediaType]; exists {
			panic("server: duplicate adapter media type " + mediaType)
		}
		r.byMediaType[mediaType] = a
		r.preference = append(r.preference, mediaType)
	}
	return r
}

func (r codecRegistry) defaultAdapter() adapter.Adapter {
	return r.byMediaType[adapter.MediaTypeProtobuf]
}

// requestAdapter selects the decoder from Content-Type. A bodyless request has
// no inbound representation, so it uses protobuf as the compatibility fallback
// and leaves Accept to select the response independently.
func (r codecRegistry) requestAdapter(contentType string, hasBody bool) (adapter.Adapter, bool) {
	if !hasBody {
		return r.defaultAdapter(), true
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, false
	}
	a, ok := r.byMediaType[strings.ToLower(mediaType)]
	return a, ok
}

type acceptRange struct {
	typePart    string
	subtypePart string
	quality     float64
	order       int
}

// responseAdapter selects an encoder from Accept. RFC 9110 media-range
// specificity decides the quality of each representation; quality then ranks
// representations. Equal choices favor the request codec, preserving the
// historical protobuf response and making an omitted/wildcard Accept follow a
// JSON request naturally.
func (r codecRegistry) responseAdapter(acceptValues []string, fallback adapter.Adapter) (adapter.Adapter, bool) {
	joined := strings.TrimSpace(strings.Join(acceptValues, ","))
	if joined == "" {
		return fallback, true
	}
	ranges := parseAccept(joined)
	if len(ranges) == 0 {
		return nil, false
	}

	mediaTypes := make([]string, 0, len(r.preference))
	if fallback != nil {
		mediaTypes = append(mediaTypes, strings.ToLower(fallback.MediaType()))
	}
	for _, mediaType := range r.preference {
		if len(mediaTypes) == 0 || mediaType != mediaTypes[0] {
			mediaTypes = append(mediaTypes, mediaType)
		}
	}

	var selected adapter.Adapter
	bestQuality := -1.0
	for _, mediaType := range mediaTypes {
		quality, ok := acceptedQuality(mediaType, ranges)
		if !ok || quality <= 0 || quality <= bestQuality {
			continue
		}
		selected = r.byMediaType[mediaType]
		bestQuality = quality
	}
	return selected, selected != nil
}

func parseAccept(value string) []acceptRange {
	parts := strings.Split(value, ",")
	ranges := make([]acceptRange, 0, len(parts))
	for order, part := range parts {
		mediaRange, params, err := mime.ParseMediaType(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		pieces := strings.Split(strings.ToLower(mediaRange), "/")
		if len(pieces) != 2 || pieces[0] == "" || pieces[1] == "" || pieces[0] == "*" && pieces[1] != "*" {
			continue
		}
		quality := 1.0
		valid := true
		for name, value := range params {
			if !strings.EqualFold(name, "q") {
				valid = false
				break
			}
			quality, err = strconv.ParseFloat(value, 64)
			if err != nil || math.IsNaN(quality) || quality < 0 || quality > 1 {
				valid = false
				break
			}
		}
		if !valid {
			continue
		}
		ranges = append(ranges, acceptRange{
			typePart:    pieces[0],
			subtypePart: pieces[1],
			quality:     quality,
			order:       order,
		})
	}
	return ranges
}

func acceptedQuality(mediaType string, ranges []acceptRange) (float64, bool) {
	parts := strings.Split(mediaType, "/")
	if len(parts) != 2 {
		return 0, false
	}
	bestSpecificity := -1
	bestOrder := len(ranges)
	quality := 0.0
	matched := false
	for _, mediaRange := range ranges {
		if mediaRange.typePart != "*" && mediaRange.typePart != parts[0] {
			continue
		}
		if mediaRange.subtypePart != "*" && mediaRange.subtypePart != parts[1] {
			continue
		}
		specificity := 0
		if mediaRange.typePart != "*" {
			specificity++
		}
		if mediaRange.subtypePart != "*" {
			specificity++
		}
		better := specificity > bestSpecificity
		if specificity == bestSpecificity {
			better = mediaRange.quality > quality ||
				mediaRange.quality == quality && mediaRange.order < bestOrder
		}
		if better {
			bestSpecificity = specificity
			bestOrder = mediaRange.order
			quality = mediaRange.quality
			matched = true
		}
	}
	return quality, matched
}
