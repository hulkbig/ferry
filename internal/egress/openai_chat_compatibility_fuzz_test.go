package egress

// Differential oracle from cbf6ace: retain compatibility while removing repeated decoding.
import (
	"bytes"
	"encoding/json"
	"fmt"
	"reflect"
	"testing"
)

func referenceChatResponseFieldIssues(body []byte) []string {
	// service_tier and system_fingerprint are operational envelope metadata;
	// they do not alter generated content, finish state, tools, or accounting.
	issues := unknownJSONFieldIssues(body, "", "id", "object", "created", "model", "choices", "usage", "error",
		"service_tier", "system_fingerprint")
	var root map[string]json.RawMessage
	_ = json.Unmarshal(body, &root)
	issues = append(issues, unknownJSONFieldIssues(root["usage"], "usage",
		"prompt_tokens", "completion_tokens", "total_tokens", "prompt_tokens_details", "completion_tokens_details")...)
	var usage map[string]json.RawMessage
	_ = json.Unmarshal(root["usage"], &usage)
	issues = append(issues, unknownJSONFieldIssues(usage["prompt_tokens_details"],
		"usage.prompt_tokens_details", "cached_tokens", "cache_write_tokens", "cache_creation_tokens", "audio_tokens",
		"image_tokens", "text_tokens")...)
	issues = append(issues, unknownJSONFieldIssues(usage["completion_tokens_details"],
		"usage.completion_tokens_details", "reasoning_tokens", "accepted_prediction_tokens",
		"rejected_prediction_tokens", "audio_tokens", "text_tokens", "compute_units")...)
	var choices []json.RawMessage
	_ = json.Unmarshal(root["choices"], &choices)
	for index, rawChoice := range choices {
		path := fmt.Sprintf("choices[%d]", index)
		issues = append(issues, unknownJSONFieldIssues(rawChoice, path,
			"index", "message", "finish_reason", "logprobs")...)
		var choice map[string]json.RawMessage
		_ = json.Unmarshal(rawChoice, &choice)
		if rawLogprobs := choice["logprobs"]; len(rawLogprobs) > 0 && string(rawLogprobs) != "null" {
			issues = append(issues, "response field "+path+".logprobs is native-only")
		}
		rawMessage := choice["message"]
		issues = append(issues, unknownJSONFieldIssues(rawMessage, path+".message",
			"role", "content", "reasoning_content", "refusal", "audio", "annotations", "tool_calls")...)
		var message map[string]json.RawMessage
		_ = json.Unmarshal(rawMessage, &message)
		var parts []json.RawMessage
		if json.Unmarshal(message["content"], &parts) == nil {
			for partIndex, rawPart := range parts {
				issues = append(issues, unknownJSONFieldIssues(rawPart,
					fmt.Sprintf("%s.message.content[%d]", path, partIndex), "type", "text")...)
			}
		}
		var calls []json.RawMessage
		_ = json.Unmarshal(message["tool_calls"], &calls)
		for callIndex, rawCall := range calls {
			callPath := fmt.Sprintf("%s.message.tool_calls[%d]", path, callIndex)
			issues = append(issues, unknownJSONFieldIssues(rawCall, callPath, "id", "type", "function")...)
			var call map[string]json.RawMessage
			_ = json.Unmarshal(rawCall, &call)
			issues = append(issues, unknownJSONFieldIssues(call["function"], callPath+".function",
				"name", "arguments")...)
		}
	}
	return issues
}

func referenceNormalizeChatChoiceContentShape(body []byte, containerKey string) ([]byte, bool) {
	var root map[string]json.RawMessage
	if json.Unmarshal(body, &root) != nil {
		return body, false
	}
	var choices []json.RawMessage
	if json.Unmarshal(root["choices"], &choices) != nil {
		return body, false
	}
	changed := false
	for index, rawChoice := range choices {
		var choice map[string]json.RawMessage
		if json.Unmarshal(rawChoice, &choice) != nil {
			continue
		}
		var container map[string]json.RawMessage
		if json.Unmarshal(choice[containerKey], &container) != nil {
			continue
		}
		rawContent, present := container["content"]
		if !present {
			continue
		}
		normalized, ok := normalizedChatTextContent(rawContent)
		if !ok {
			continue
		}
		container["content"] = normalized
		normalizedContainer, err := json.Marshal(container)
		if err != nil {
			continue
		}
		choice[containerKey] = normalizedContainer
		normalizedChoice, err := json.Marshal(choice)
		if err != nil {
			continue
		}
		choices[index] = normalizedChoice
		changed = true
	}
	if !changed {
		return body, false
	}
	normalizedChoices, err := json.Marshal(choices)
	if err != nil {
		return body, false
	}
	root["choices"] = normalizedChoices
	normalizedBody, err := json.Marshal(root)
	if err != nil {
		return body, false
	}
	return normalizedBody, true
}

func FuzzChatInspectionCompatibility(f *testing.F) {
	for _, seed := range []string{
		`{"choices":[{"message":{"content":"hello"}}]}`,
		`{"choices":[{"message":{"content":{"text":"hello"}},"Message":{"content":"ignored"}}]}`,
		`{"choices":[{"message":{"content":{"z":2,"a":1}}}]}`,
		`{"future":1e999,"choices":[{"message":{"content":"hello","extra":true}}]}`,
		`{"choices":[{"logprobs":{},"message":{"content":[{"type":"text","text":"hello","future":1}],"tool_calls":[{"type":"function","function":{"name":"tool","arguments":"{}","future":1}}]}}],"usage":{"prompt_tokens_details":{"future":true}}}`,
		`{"choices":[null,1,{"delta":{"content":["a","b"]}}]}`,
		`{"choices":null}`, `null`, `{}`, `[]`, `invalid`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, body []byte) {
		if len(body) > 131072 {
			t.Skip()
		}
		normalized, changed, issues := inspectChatResponse(body)
		wantBody, wantChanged := referenceNormalizeChatChoiceContentShape(body, "message")
		if string(normalized) != string(wantBody) || changed != wantChanged || !reflect.DeepEqual(issues, referenceChatResponseFieldIssues(wantBody)) {
			t.Fatalf("combined inspection differs: changed=%v/%v, body=%q/%q, issues=%v/%v", changed, wantChanged, normalized, wantBody, issues, referenceChatResponseFieldIssues(wantBody))
		}

		if a, b := chatResponseFieldIssues(body), referenceChatResponseFieldIssues(body); !reflect.DeepEqual(a, b) {
			t.Fatalf("issues differ: %q != %q input=%s", a, b, body)
		}
		for _, key := range []string{"message", "delta"} {
			a, ac := normalizeChatChoiceContentShape(body, key)
			b, bc := referenceNormalizeChatChoiceContentShape(body, key)
			if ac != bc || !bytes.Equal(a, b) {
				t.Fatalf("normalization differs key=%s input=%s got=%s want=%s", key, body, a, b)
			}
		}
	})
}
