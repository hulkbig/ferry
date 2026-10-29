package egress

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/url"
	"strings"

	"ferry/internal/domain"
)

// isZhipuOpenAIChatEndpoint identifies the two official Zhipu/Z.AI OpenAI
// compatible API surfaces.  Provider-specific normalization must never leak
// into generic OpenAI-compatible connections, where exact native passthrough
// remains the correct behavior.
func isZhipuOpenAIChatEndpoint(endpoint *domain.Endpoint) bool {
	if endpoint == nil {
		return false
	}
	for _, rawURL := range []string{endpoint.FullURL, endpoint.BaseURL} {
		parsed, err := url.Parse(strings.TrimSpace(rawURL))
		if err != nil {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if host != "open.bigmodel.cn" && host != "api.z.ai" {
			continue
		}
		path := strings.TrimSuffix(parsed.EscapedPath(), "/")
		if strings.Contains(path, "/api/paas/v4") || strings.Contains(path, "/api/coding/paas/v4") {
			return true
		}
	}
	return false
}

// zhipuNativeChatBody translates the subset of OpenAI Chat fields that have
// documented, semantics-preserving equivalents in Zhipu's Chat API.  It also
// rejects OpenAI-only behavior locally instead of forwarding it and exposing
// Zhipu's opaque 1214 "illegal field parameter" response.
func zhipuNativeChatBody(rc *domain.RequestContract, upstreamModel string) ([]byte, bool, error) {
	if rc == nil || rc.Origin != domain.ProtocolOpenAIChat || len(rc.Native) == 0 || upstreamModel == "" {
		return nil, false, nil
	}
	var body map[string]json.RawMessage
	if err := json.Unmarshal(rc.Native, &body); err != nil {
		return nil, true, fmt.Errorf("zhipu chat compatibility: invalid preserved request: %w", err)
	}
	if _, ok := body["model"]; !ok {
		return nil, false, nil
	}
	body["model"] = mustJSON(upstreamModel)
	body["stream"] = mustJSON(rc.Stream)
	out, err := json.Marshal(body)
	if err != nil {
		return nil, true, fmt.Errorf("zhipu chat compatibility: encode request: %w", err)
	}
	return out, true, nil
}

func normalizeZhipuChatBody(body map[string]json.RawMessage, upstreamModel string) error {
	if err := normalizeZhipuMessages(body); err != nil {
		return err
	}
	if err := renameZhipuMaxTokens(body); err != nil {
		return err
	}
	if err := normalizeZhipuStore(body); err != nil {
		return err
	}
	if err := normalizeZhipuStreamOptions(body); err != nil {
		return err
	}
	if err := normalizeZhipuTools(body); err != nil {
		return err
	}
	if err := normalizeZhipuReasoning(body, upstreamModel); err != nil {
		return err
	}
	if err := normalizeZhipuParallelTools(body); err != nil {
		return err
	}
	if err := normalizeZhipuToolChoice(body); err != nil {
		return err
	}
	if err := normalizeZhipuResponseFormat(body); err != nil {
		return err
	}
	if err := normalizeZhipuStop(body); err != nil {
		return err
	}
	if err := normalizeZhipuUser(body); err != nil {
		return err
	}
	delete(body, "prompt_cache_key") // operational cache hint, not model semantics
	if err := normalizeZhipuOpenAIOnlyFields(body); err != nil {
		return err
	}
	return rejectUndocumentedZhipuFields(body)
}

func normalizeZhipuMessages(body map[string]json.RawMessage) error {
	var messages []map[string]json.RawMessage
	if err := json.Unmarshal(body["messages"], &messages); err != nil {
		return fmt.Errorf("messages must be an array of objects")
	}
	hasSystem, hasDeveloper := false, false
	for index, message := range messages {
		var role string
		if err := json.Unmarshal(message["role"], &role); err != nil {
			return fmt.Errorf("messages[%d].role must be a string", index)
		}
		switch role {
		case "system":
			hasSystem = true
		case "developer":
			hasDeveloper = true
		case "user", "assistant", "tool":
		default:
			return fmt.Errorf("messages[%d].role %q is unsupported; Zhipu accepts system, user, assistant, or tool", index, role)
		}
	}
	if hasSystem && hasDeveloper {
		return fmt.Errorf("developer and system roles cannot both be represented with Zhipu's single system-role priority")
	}
	for index, message := range messages {
		var role string
		_ = json.Unmarshal(message["role"], &role)
		if role == "developer" {
			message["role"] = mustJSON("system")
		}
		if content, ok := message["content"]; ok {
			normalized, changed, err := zhipuTextContent(content)
			if err != nil {
				return fmt.Errorf("messages[%d].content: %w", index, err)
			}
			if changed {
				message["content"] = normalized
			}
		}
	}
	body["messages"] = mustJSON(messages)
	return nil
}

// zhipuTextContent collapses OpenAI text-part arrays to the plain string form
// documented for ordinary Zhipu text models. Multimodal arrays are retained.
func zhipuTextContent(raw json.RawMessage) (json.RawMessage, bool, error) {
	var parts []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &parts); err != nil {
		return raw, false, nil
	}
	var text strings.Builder
	for index, part := range parts {
		var kind string
		if err := json.Unmarshal(part["type"], &kind); err != nil {
			return nil, false, fmt.Errorf("part %d type must be a string", index)
		}
		if kind != "text" {
			return raw, false, nil
		}
		var value string
		if err := json.Unmarshal(part["text"], &value); err != nil {
			return nil, false, fmt.Errorf("part %d text must be a string", index)
		}
		text.WriteString(value)
	}
	return mustJSON(text.String()), true, nil
}

func renameZhipuMaxTokens(body map[string]json.RawMessage) error {
	completion, ok := body["max_completion_tokens"]
	if !ok {
		return nil
	}
	completionValue, err := rawInt64(completion)
	if err != nil {
		return fmt.Errorf("max_completion_tokens must be an integer")
	}
	if current, exists := body["max_tokens"]; exists {
		currentValue, err := rawInt64(current)
		if err != nil {
			return fmt.Errorf("max_tokens must be an integer")
		}
		if currentValue != completionValue {
			return fmt.Errorf("max_tokens and max_completion_tokens disagree")
		}
	} else {
		body["max_tokens"] = completion
	}
	delete(body, "max_completion_tokens")
	return nil
}

func normalizeZhipuStore(body map[string]json.RawMessage) error {
	raw, ok := body["store"]
	if !ok {
		return nil
	}
	value, isNull, err := rawBool(raw)
	if err != nil {
		return fmt.Errorf("store must be a boolean")
	}
	if !isNull && value {
		return fmt.Errorf("store=true is unsupported because Zhipu Chat has no equivalent server-side storage contract")
	}
	delete(body, "store")
	return nil
}

func normalizeZhipuStreamOptions(body map[string]json.RawMessage) error {
	raw, ok := body["stream_options"]
	if !ok || isJSONNull(raw) {
		delete(body, "stream_options")
		return nil
	}
	var options map[string]json.RawMessage
	if err := json.Unmarshal(raw, &options); err != nil {
		return fmt.Errorf("stream_options must be an object")
	}
	for key := range options {
		if key != "include_usage" {
			return fmt.Errorf("stream_options.%s is unsupported by Zhipu Chat", key)
		}
	}
	if include, exists := options["include_usage"]; exists {
		value, isNull, err := rawBool(include)
		if err != nil {
			return fmt.Errorf("stream_options.include_usage must be a boolean")
		}
		if !isNull && !value {
			return fmt.Errorf("stream_options.include_usage=false cannot be preserved because Zhipu returns usage at completion")
		}
	}
	delete(body, "stream_options")
	return nil
}

func normalizeZhipuTools(body map[string]json.RawMessage) error {
	raw, ok := body["tools"]
	if !ok || isJSONNull(raw) {
		return nil
	}
	var tools []map[string]json.RawMessage
	if err := json.Unmarshal(raw, &tools); err != nil {
		return fmt.Errorf("tools must be an array of objects")
	}
	for index, tool := range tools {
		functionRaw, exists := tool["function"]
		if !exists || isJSONNull(functionRaw) {
			continue // Zhipu also documents retrieval, web-search, and MCP tools.
		}
		var function map[string]json.RawMessage
		if err := json.Unmarshal(functionRaw, &function); err != nil {
			return fmt.Errorf("tools[%d].function must be an object", index)
		}
		if strictRaw, exists := function["strict"]; exists {
			strict, isNull, err := rawBool(strictRaw)
			if err != nil {
				return fmt.Errorf("tools[%d].function.strict must be a boolean", index)
			}
			if !isNull && strict {
				return fmt.Errorf("tools[%d].function.strict=true is unsupported by Zhipu Chat", index)
			}
			delete(function, "strict")
		}
		tool["function"] = mustJSON(function)
	}
	body["tools"] = mustJSON(tools)
	return nil
}

func normalizeZhipuReasoning(body map[string]json.RawMessage, upstreamModel string) error {
	raw, ok := body["reasoning_effort"]
	if !ok || isJSONNull(raw) {
		delete(body, "reasoning_effort")
		return nil
	}
	var effort string
	if err := json.Unmarshal(raw, &effort); err != nil {
		return fmt.Errorf("reasoning_effort must be a string")
	}
	model := strings.ToLower(strings.TrimSpace(upstreamModel))
	if !strings.HasPrefix(model, "glm-5.2") && !strings.HasPrefix(model, "glm-5.3") {
		return fmt.Errorf("reasoning_effort is unsupported by model %q; Zhipu documents it for GLM-5.2 and newer", upstreamModel)
	}
	switch effort {
	case "low", "high", "max":
	case "medium":
		body["reasoning_effort"] = mustJSON("high")
	case "xhigh":
		body["reasoning_effort"] = mustJSON("max")
	case "none", "minimal":
		if thinking, exists := body["thinking"]; exists && !isDisabledZhipuThinking(thinking) {
			return fmt.Errorf("reasoning_effort=%s conflicts with enabled thinking", effort)
		}
		body["thinking"] = mustJSON(map[string]string{"type": "disabled"})
		delete(body, "reasoning_effort")
	default:
		return fmt.Errorf("reasoning_effort %q is unsupported by Zhipu Chat", effort)
	}
	return nil
}

func isDisabledZhipuThinking(raw json.RawMessage) bool {
	var thinking struct {
		Type string `json:"type"`
	}
	return json.Unmarshal(raw, &thinking) == nil && thinking.Type == "disabled"
}

func normalizeZhipuParallelTools(body map[string]json.RawMessage) error {
	raw, ok := body["parallel_tool_calls"]
	if !ok || isJSONNull(raw) {
		delete(body, "parallel_tool_calls")
		return nil
	}
	parallel, _, err := rawBool(raw)
	if err != nil {
		return fmt.Errorf("parallel_tool_calls must be a boolean")
	}
	if parallel {
		return fmt.Errorf("parallel_tool_calls=true is unsupported by this Zhipu Chat dialect")
	}
	delete(body, "parallel_tool_calls")
	return nil
}

func normalizeZhipuToolChoice(body map[string]json.RawMessage) error {
	raw, ok := body["tool_choice"]
	if !ok || isJSONNull(raw) {
		delete(body, "tool_choice")
		return nil
	}
	var choice string
	if err := json.Unmarshal(raw, &choice); err != nil {
		return fmt.Errorf("named/object tool_choice is unsupported; Zhipu Chat documents only auto")
	}
	switch choice {
	case "auto":
		return nil
	case "none":
		delete(body, "tools")
		delete(body, "tool_choice")
		return nil
	default:
		return fmt.Errorf("tool_choice=%q is unsupported; Zhipu Chat documents only auto", choice)
	}
}

func normalizeZhipuResponseFormat(body map[string]json.RawMessage) error {
	raw, ok := body["response_format"]
	if !ok || isJSONNull(raw) {
		delete(body, "response_format")
		return nil
	}
	var format map[string]json.RawMessage
	if err := json.Unmarshal(raw, &format); err != nil {
		return fmt.Errorf("response_format must be an object")
	}
	var kind string
	if err := json.Unmarshal(format["type"], &kind); err != nil {
		return fmt.Errorf("response_format.type must be a string")
	}
	if kind != "text" && kind != "json_object" {
		return fmt.Errorf("response_format.type=%q is unsupported; Zhipu Chat supports text or json_object", kind)
	}
	for field := range format {
		if field != "type" {
			return fmt.Errorf("response_format.%s is unsupported by Zhipu Chat", field)
		}
	}
	return nil
}

func normalizeZhipuStop(body map[string]json.RawMessage) error {
	raw, ok := body["stop"]
	if !ok || isJSONNull(raw) {
		delete(body, "stop")
		return nil
	}
	var one string
	if json.Unmarshal(raw, &one) == nil {
		body["stop"] = mustJSON([]string{one})
		return nil
	}
	var many []string
	if err := json.Unmarshal(raw, &many); err != nil {
		return fmt.Errorf("stop must be a string or string array")
	}
	if len(many) > 4 {
		return fmt.Errorf("stop has %d entries; Zhipu Chat supports at most 4", len(many))
	}
	return nil
}

func normalizeZhipuUser(body map[string]json.RawMessage) error {
	raw, ok := body["user"]
	if !ok || isJSONNull(raw) {
		delete(body, "user")
		return nil
	}
	var user string
	if err := json.Unmarshal(raw, &user); err != nil {
		return fmt.Errorf("user must be a string")
	}
	if existing, exists := body["user_id"]; exists {
		var userID string
		if err := json.Unmarshal(existing, &userID); err != nil || userID != user {
			return fmt.Errorf("user and user_id disagree")
		}
	} else {
		body["user_id"] = mustJSON(user)
	}
	delete(body, "user")
	return nil
}

func normalizeZhipuOpenAIOnlyFields(body map[string]json.RawMessage) error {
	if raw, ok := body["n"]; ok {
		value, err := rawInt64(raw)
		if err != nil || value != 1 {
			return fmt.Errorf("n is unsupported by Zhipu Chat unless it is 1")
		}
		delete(body, "n")
	}
	for _, field := range []string{"frequency_penalty", "presence_penalty"} {
		if raw, ok := body[field]; ok {
			var value float64
			if err := json.Unmarshal(raw, &value); err != nil || value != 0 {
				return fmt.Errorf("%s is unsupported by Zhipu Chat unless it is 0", field)
			}
			delete(body, field)
		}
	}
	if raw, ok := body["logprobs"]; ok {
		value, isNull, err := rawBool(raw)
		if err != nil || (!isNull && value) {
			return fmt.Errorf("logprobs is unsupported by Zhipu Chat")
		}
		delete(body, "logprobs")
	}
	if raw, ok := body["modalities"]; ok {
		var modalities []string
		if err := json.Unmarshal(raw, &modalities); err != nil || len(modalities) != 1 || modalities[0] != "text" {
			return fmt.Errorf("modalities is unsupported by Zhipu Chat unless it is [\"text\"]")
		}
		delete(body, "modalities")
	}
	for _, field := range []string{
		"audio", "prediction", "seed", "service_tier", "metadata", "logit_bias",
		"top_logprobs", "web_search_options", "verbosity", "safety_identifier", "top_k",
	} {
		if raw, ok := body[field]; ok {
			if isJSONNull(raw) {
				delete(body, field)
				continue
			}
			return fmt.Errorf("field %s is unsupported by Zhipu Chat", field)
		}
	}
	return nil
}

func rejectUndocumentedZhipuFields(body map[string]json.RawMessage) error {
	documented := map[string]bool{
		"model": true, "messages": true, "stream": true, "thinking": true,
		"reasoning_effort": true, "do_sample": true, "temperature": true,
		"top_p": true, "max_tokens": true, "tool_stream": true, "tools": true,
		"tool_choice": true, "stop": true, "response_format": true,
		"request_id": true, "user_id": true,
	}
	for field, raw := range body {
		if documented[field] {
			continue
		}
		if isJSONNull(raw) {
			delete(body, field)
			continue
		}
		return fmt.Errorf("field %s is not documented by the Zhipu Chat API", field)
	}
	return nil
}

func rawBool(raw json.RawMessage) (value, isNull bool, err error) {
	if isJSONNull(raw) {
		return false, true, nil
	}
	err = json.Unmarshal(raw, &value)
	return value, false, err
}

func rawInt64(raw json.RawMessage) (int64, error) {
	var value int64
	err := json.Unmarshal(raw, &value)
	return value, err
}

func isJSONNull(raw json.RawMessage) bool {
	return bytes.Equal(bytes.TrimSpace(raw), []byte("null"))
}
