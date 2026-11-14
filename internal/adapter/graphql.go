package adapter

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"unicode/utf8"

	"github.com/alternet-dev/wavefront/internal/wireerror"
)

const (
	graphQLPath              = "/graphql"
	mediaTypeJSON            = "application/json"
	mediaTypeGraphQLResponse = "application/graphql-response+json"
)

// GraphQL implements the JSON envelope defined by GraphQL over HTTP. Unlike
// ProtoJSON, it does not use a binding's route or fixed message descriptors:
// every request targets POST /graphql and the upstream response already has
// the shape selected by the GraphQL document.
//
// Document syntax and schema validation belong to the upstream GraphQL server.
// This adapter validates only the HTTP request and response envelope so broken
// JSON is reported through wavefront's typed error path.
type GraphQL struct{}

func NewGraphQL() *GraphQL { return &GraphQL{} }

func (*GraphQL) DecodeRequest(_ Binding, in []byte) (UpstreamCall, *wireerror.Error) {
	if err := validateGraphQLRequest(in); err != nil {
		return UpstreamCall{}, wireerror.DecodeFailed("request body is not a valid GraphQL-over-HTTP envelope: " + err.Error())
	}
	return UpstreamCall{
		Method:      "POST",
		Path:        graphQLPath,
		Body:        in,
		ContentType: mediaTypeJSON,
	}, nil
}

func (*GraphQL) EncodeResponse(_ Binding, upstreamJSON []byte) ([]byte, string, *wireerror.Error) {
	return encodeGraphQLResponse(upstreamJSON)
}

// EncodeError handles a declared non-success response the same way as a
// success response. GraphQL's response envelope, not the HTTP status, carries
// the request-shaped data and errors members.
func (*GraphQL) EncodeError(_ Binding, _ int, upstreamJSON []byte) ([]byte, string, *wireerror.Error) {
	return encodeGraphQLResponse(upstreamJSON)
}

func encodeGraphQLResponse(in []byte) ([]byte, string, *wireerror.Error) {
	if err := validateGraphQLResponse(in); err != nil {
		return nil, "", wireerror.UpstreamError("upstream body is not a valid GraphQL response envelope: " + err.Error())
	}
	return in, mediaTypeGraphQLResponse, nil
}

func validateGraphQLRequest(in []byte) error {
	obj, err := decodeJSONObject(in)
	if err != nil {
		return err
	}

	queryRaw, ok := obj["query"]
	if !ok {
		return errors.New("query is required")
	}
	var query string
	if err := json.Unmarshal(queryRaw, &query); err != nil {
		return errors.New("query must be a string")
	}
	if raw, ok := obj["operationName"]; ok && !isJSONNull(raw) {
		var operationName string
		if err := json.Unmarshal(raw, &operationName); err != nil {
			return errors.New("operationName must be a string or null")
		}
	}
	for _, name := range []string{"variables", "extensions"} {
		if raw, ok := obj[name]; ok {
			if err := validateNullableObject(raw); err != nil {
				return fmt.Errorf("%s must be an object or null", name)
			}
		}
	}
	return nil
}

func validateGraphQLResponse(in []byte) error {
	obj, err := decodeJSONObject(in)
	if err != nil {
		return err
	}
	for name := range obj {
		if name != "data" && name != "errors" && name != "extensions" {
			return fmt.Errorf("%s is not a permitted response member", name)
		}
	}

	dataRaw, hasData := obj["data"]
	errorsRaw, hasErrors := obj["errors"]
	if !hasData && !hasErrors {
		return errors.New("data or errors is required")
	}
	if hasData && isJSONNull(dataRaw) && !hasErrors {
		return errors.New("errors is required when data is null")
	}
	if hasData && !isJSONNull(dataRaw) {
		var data map[string]json.RawMessage
		if err := json.Unmarshal(dataRaw, &data); err != nil || data == nil {
			return errors.New("data must be an object or null")
		}
	}
	if hasErrors {
		var entries []json.RawMessage
		if err := json.Unmarshal(errorsRaw, &entries); err != nil || len(entries) == 0 {
			return errors.New("errors must be a non-empty array")
		}
		for _, entry := range entries {
			errorObject, err := decodeJSONObject(entry)
			if err != nil {
				return errors.New("each errors entry must be an object")
			}
			messageRaw, ok := errorObject["message"]
			if !ok {
				return errors.New("each errors entry must contain message")
			}
			var message string
			if err := json.Unmarshal(messageRaw, &message); err != nil {
				return errors.New("each errors entry message must be a string")
			}
			if extensionsRaw, ok := errorObject["extensions"]; ok {
				var extensions map[string]json.RawMessage
				if err := json.Unmarshal(extensionsRaw, &extensions); err != nil || extensions == nil {
					return errors.New("each errors entry extensions must be an object")
				}
			}
			if locationsRaw, ok := errorObject["locations"]; ok {
				if err := validateErrorLocations(locationsRaw); err != nil {
					return err
				}
			}
			if pathRaw, ok := errorObject["path"]; ok {
				if err := validateErrorPath(pathRaw); err != nil {
					return err
				}
			}
		}
	}
	if extensionsRaw, ok := obj["extensions"]; ok {
		var extensions map[string]json.RawMessage
		if err := json.Unmarshal(extensionsRaw, &extensions); err != nil || extensions == nil {
			return errors.New("extensions must be an object")
		}
	}
	return nil
}

func decodeJSONObject(in []byte) (map[string]json.RawMessage, error) {
	if err := validateJSONDocument(in); err != nil {
		return nil, err
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(in, &obj); err != nil {
		return nil, errors.New("body must be one JSON object")
	}
	if obj == nil {
		return nil, errors.New("body must be one JSON object")
	}
	return obj, nil
}

func validateErrorLocations(in []byte) error {
	var locations []json.RawMessage
	if err := json.Unmarshal(in, &locations); err != nil {
		return errors.New("each errors entry locations must be an array")
	}
	for _, location := range locations {
		obj, err := decodeJSONObject(location)
		if err != nil {
			return errors.New("each errors entry location must be an object")
		}
		for _, name := range []string{"line", "column"} {
			raw, ok := obj[name]
			if !ok || !isPositiveInteger(raw) {
				return fmt.Errorf("each errors entry location %s must be a positive integer", name)
			}
		}
	}
	return nil
}

func validateErrorPath(in []byte) error {
	var path []json.RawMessage
	if err := json.Unmarshal(in, &path); err != nil {
		return errors.New("each errors entry path must be an array")
	}
	for _, segment := range path {
		var stringSegment string
		if err := json.Unmarshal(segment, &stringSegment); err == nil {
			continue
		}
		var integerSegment int64
		if err := json.Unmarshal(segment, &integerSegment); err == nil {
			continue
		}
		return errors.New("each errors entry path segments must be strings or integers")
	}
	return nil
}

func isPositiveInteger(in []byte) bool {
	var value int64
	return json.Unmarshal(in, &value) == nil && value > 0
}

func validateJSONDocument(in []byte) error {
	if !utf8.Valid(in) {
		return errors.New("body must be valid UTF-8")
	}
	decoder := json.NewDecoder(bytes.NewReader(in))
	if err := validateJSONValue(decoder); err != nil {
		return errors.New("body must be valid JSON")
	}
	if _, err := decoder.Token(); err != io.EOF {
		return errors.New("body must contain one JSON value")
	}
	return nil
}

func validateJSONValue(decoder *json.Decoder) error {
	token, err := decoder.Token()
	if err != nil {
		return err
	}
	switch token := token.(type) {
	case json.Delim:
		switch token {
		case '{':
			members := make(map[string]struct{})
			for decoder.More() {
				nameToken, err := decoder.Token()
				if err != nil {
					return err
				}
				name, ok := nameToken.(string)
				if !ok {
					return errors.New("object member name is not a string")
				}
				if _, duplicate := members[name]; duplicate {
					return fmt.Errorf("duplicate object member %q", name)
				}
				members[name] = struct{}{}
				if err := validateJSONValue(decoder); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		case '[':
			for decoder.More() {
				if err := validateJSONValue(decoder); err != nil {
					return err
				}
			}
			_, err := decoder.Token()
			return err
		default:
			return errors.New("unexpected JSON delimiter")
		}
	}
	return nil
}

func validateNullableObject(in []byte) error {
	if isJSONNull(in) {
		return nil
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(in, &obj); err != nil || obj == nil {
		return errors.New("not an object")
	}
	return nil
}

func isJSONNull(in []byte) bool {
	return bytes.Equal(bytes.TrimSpace(in), []byte("null"))
}
