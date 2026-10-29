package egress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"ferry/internal/domain"
)

// ProviderAdapter is the internal, built-in request normalization boundary.
// Protocol adapters own wire encoding and stream semantics; providers only
// repair documented dialect differences. Authentication, admission, retries
// and capability evidence remain outside this interface. Errors are fatal to
// preparation: a rejected normalization must never silently use the old body.
// Implementations must not mutate body or broaden the schema's accepted values.
type ProviderAdapter interface {
	NormalizeRequest(protocol domain.Protocol, native bool, body []byte) ([]byte, error)
}

type schemaProviderAdapter struct{ kimi, deepseek bool }
type zhipuProviderAdapter struct{ model string }

func providerAdapter(endpoint *domain.Endpoint, model string, protocol domain.Protocol) ProviderAdapter {
	// Use the actual resolved destination, including FullURL precedence. A known
	// BaseURL must not authorize repairs on a different FullURL host.
	suffix := "/chat/completions"
	if protocol == domain.ProtocolAnthropic {
		suffix = "/messages"
	}
	if protocol == domain.ProtocolOpenAIResponses {
		suffix = "/responses"
	}
	address, err := endpoint.RequestURL(suffix)
	if err != nil {
		return nil
	}
	u, err := url.Parse(address)
	if err != nil {
		return nil
	}
	host := strings.ToLower(u.Hostname())
	switch host {
	case "api.moonshot.ai", "api.moonshot.cn", "api.kimi.com", "api.kimi.ai":
		return schemaProviderAdapter{kimi: true}
	case "api.deepseek.com":
		return schemaProviderAdapter{deepseek: true}
	}
	actual := &domain.Endpoint{FullURL: address}
	if protocol == domain.ProtocolOpenAIChat && isZhipuOpenAIChatEndpoint(actual) {
		return zhipuProviderAdapter{model: model}
	}
	return nil
}

func normalizeProviderRequest(base Base, protocol domain.Protocol, rc *domain.RequestContract, body []byte) ([]byte, error) {
	adapter := providerAdapter(base.Endpoint, base.Model, protocol)
	if adapter == nil {
		return body, nil
	}
	native := rc != nil && rc.Origin == protocol && len(rc.Native) != 0
	return adapter.NormalizeRequest(protocol, native, body)
}

func (z zhipuProviderAdapter) NormalizeRequest(protocol domain.Protocol, native bool, body []byte) ([]byte, error) {
	// Canonical Zhipu fields are already mapped by the Chat encoder. The raw
	// native path retains its existing stricter field-rejection contract.
	if protocol != domain.ProtocolOpenAIChat || !native {
		return body, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(body, &fields); err != nil {
		return nil, err
	}
	if err := normalizeZhipuChatBody(fields, z.model); err != nil {
		return nil, fmt.Errorf("zhipu chat compatibility: %w", err)
	}
	return json.Marshal(fields)
}

func (s schemaProviderAdapter) NormalizeRequest(protocol domain.Protocol, _ bool, body []byte) ([]byte, error) {
	if !json.Valid(body) {
		return nil, fmt.Errorf("provider normalization requires valid JSON")
	}
	return rewriteJSONChildren(body, func(key string, value []byte) []byte {
		if key != "tools" || !bytes.HasPrefix(bytes.TrimSpace(value), []byte("[")) {
			return value
		}
		return rewriteJSONChildren(value, func(_ string, tool []byte) []byte {
			var header struct {
				Type string `json:"type"`
			}
			if json.Unmarshal(tool, &header) != nil {
				return tool
			}
			if protocol == domain.ProtocolAnthropic {
				if header.Type != "" && header.Type != "custom" {
					return tool
				}
			} else if header.Type != "function" {
				return tool
			}

			return rewriteJSONChildren(tool, func(key string, value []byte) []byte {
				switch {
				case protocol == domain.ProtocolOpenAIChat && key == "function":
					return rewriteJSONChildren(value, func(key string, value []byte) []byte {
						if key == "parameters" {
							return s.schema(value)
						}
						return value
					})
				case protocol == domain.ProtocolAnthropic && key == "input_schema",
					protocol == domain.ProtocolOpenAIResponses && key == "parameters":
					return s.schema(value)
				}
				return value
			})
		})
	}), nil
}

// rewriteJSONChildren uses Decoder offsets to replace only changed child
// values. This preserves ordering, whitespace, number spelling and prompt
// bytes outside the repaired schema, without a second JSON parser dependency.
func rewriteJSONChildren(raw []byte, visit func(string, []byte) []byte) []byte {
	dec := json.NewDecoder(bytes.NewReader(raw))
	token, err := dec.Token()
	if err != nil {
		return raw
	}
	delim, ok := token.(json.Delim)
	if !ok || delim != '{' && delim != '[' {
		return raw
	}
	var out []byte
	last := 0
	for dec.More() {
		key := ""
		if delim == '{' {
			token, err = dec.Token()
			if err != nil {
				return raw
			}
			key, ok = token.(string)
			if !ok {
				return raw
			}
		}
		var value json.RawMessage
		if dec.Decode(&value) != nil {
			return raw
		}
		end := int(dec.InputOffset())
		start := end - len(value)
		replacement := visit(key, value)
		if !bytes.Equal(value, replacement) {
			out = append(out, raw[last:start]...)
			out = append(out, replacement...)
			last = end
		}
	}
	if last == 0 {
		return raw
	}
	return append(out, raw[last:]...)
}

func (s schemaProviderAdapter) schema(raw []byte) []byte {
	var fields map[string]json.RawMessage
	if json.Unmarshal(raw, &fields) != nil || fields == nil {
		return raw
	}
	rewritten := rewriteJSONChildren(raw, func(key string, value []byte) []byte {
		switch key {
		case "properties", "patternProperties", "$defs", "$def", "definitions", "dependentSchemas":
			return rewriteJSONChildren(value, func(_ string, child []byte) []byte { return s.schema(child) })
		case "allOf", "anyOf", "oneOf", "prefixItems":
			return rewriteJSONChildren(value, func(_ string, child []byte) []byte { return s.schema(child) })
		case "items":
			if bytes.HasPrefix(bytes.TrimSpace(value), []byte("[")) {
				return rewriteJSONChildren(value, func(_ string, child []byte) []byte { return s.schema(child) })
			}
			return s.schema(value)
		case "additionalProperties", "additionalItems", "contains", "propertyNames", "not", "if", "then", "else", "unevaluatedProperties", "unevaluatedItems", "contentSchema":
			return s.schema(value)
		case "pattern":
			if s.deepseek {
				var pattern string
				if json.Unmarshal(value, &pattern) == nil {
					normalized := normalizeNULPattern(pattern)
					if normalized != pattern {
						encoded, _ := json.Marshal(normalized)
						return encoded
					}
				}
			}
		}
		return value
	})
	// Deliberately do not infer from const, mixed scalar/null, arrays or objects.
	// The official MFJS enum subset documents string and number values only.
	if s.kimi && fields["type"] == nil {
		if inferred := kimiEnumType(fields["enum"]); inferred != "" {
			start := bytes.IndexByte(rewritten, '{') + 1
			out := append([]byte(nil), rewritten[:start]...)
			out = append(out, []byte(`"type":"`+inferred+`",`)...)
			return append(out, rewritten[start:]...)
		}
	}
	return rewritten
}

func kimiEnumType(raw []byte) string {
	var values []any
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	if dec.Decode(&values) != nil || len(values) == 0 {
		return ""
	}
	result := ""
	for _, value := range values {
		kind := ""
		switch v := value.(type) {
		case string:
			kind = "string"
		case json.Number:
			kind = "number"
			// Preserve arbitrary precision and avoid expanding huge exponents.
			if !strings.ContainsAny(string(v), ".eE") {
				kind = "integer"
			}
		default:
			return ""
		}
		if result == "" {
			result = kind
			continue
		}
		if result != kind {
			if result == "string" || kind == "string" {
				return ""
			}
			result = "number"
		}
	}
	return result
}

func normalizeNULPattern(pattern string) string {
	var out strings.Builder
	for index := 0; index < len(pattern); {
		if pattern[index] != '\\' || index+1 == len(pattern) {
			out.WriteByte(pattern[index])
			index++
			continue
		}
		// Consume each regex escape as a pair: escaped backslashes are literals.
		// Decimal/octal sequences are not a standalone NUL escape.
		if pattern[index+1] == '0' && (index+2 == len(pattern) || pattern[index+2] < '0' || pattern[index+2] > '9') {
			out.WriteString(`\u0000`)
		} else {
			out.WriteString(pattern[index : index+2])
		}
		index += 2
	}
	return out.String()
}
