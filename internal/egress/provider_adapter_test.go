package egress

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"ferry/internal/domain"
)

func TestProviderSchemaNormalizationIsSurgical(t *testing.T) {
	for _, tc := range []struct{ name, host, before, after string }{
		{"kimi", "api.moonshot.ai", `{"enum":["small","large"]}`, `{"type":"string","enum":["small","large"]}`},
		{"deepseek", "api.deepseek.com", `{"type":"string","pattern":"^[^\\0]+$"}`, `{"type":"string","pattern":"^[^\\u0000]+$"}`},
	} {
		for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic} {
			t.Run(tc.name+"/"+string(protocol), func(t *testing.T) {
				tool := `{"type":"function","function":{"name":"pick","parameters":` + tc.before + `}}`
				if protocol == domain.ProtocolOpenAIResponses {
					tool = `{"type":"function","name":"pick","parameters":` + tc.before + `}`
				}
				if protocol == domain.ProtocolAnthropic {
					tool = `{"name":"pick","input_schema":` + tc.before + `}`
				}
				// Unrelated enum and pattern data, unusual numeric spelling, whitespace,
				// HTML and UTF-8 must survive byte-for-byte, including inside messages.
				original := []byte("{\n  \"temperature\":1.00e-1,\"messages\":[{\"role\":\"user\",\"content\":\"<你好>&\"}],\"metadata\":" + tc.before + ", \"tools\": [ " + tool + " ] }")
				copyOf := append([]byte(nil), original...)
				adapter := providerAdapter(&domain.Endpoint{BaseURL: "https://" + tc.host + "/v1"}, "m", protocol)
				got, err := adapter.NormalizeRequest(protocol, true, original)
				want := strings.Replace(string(original), tool, strings.Replace(tool, tc.before, tc.after, 1), 1)
				if err != nil || string(got) != want {
					t.Fatalf("got %s err=%v; want %s", got, err, want)
				}
				if !bytes.Equal(original, copyOf) {
					t.Fatal("mutated caller bytes")
				}
				twice, err := adapter.NormalizeRequest(protocol, true, got)
				if err != nil || !bytes.Equal(got, twice) {
					t.Fatal("normalization is not idempotent")
				}
			})
		}
	}
}

func TestKimiEnumInferenceBoundaries(t *testing.T) {
	for _, tc := range []struct{ schema, typ string }{
		{`{"enum":[1,9223372036854775808123]}`, "integer"},
		{`{"enum":[1,2.5]}`, "number"},
		{`{"enum":[1e10000000]}`, "number"},
		{`{"enum":["x","y"]}`, "string"},
		{`{"enum":["x",2]}`, ""}, {`{"enum":[true,false]}`, ""},
		{`{"enum":[null,"x"]}`, ""}, {`{"enum":[]}`, ""},
		{`{"enum":[{"type":"x"}]}`, ""}, {`{"const":"x"}`, ""},
		{`{"type":null,"enum":["x"]}`, ""},
		{`{"type":"number","enum":["x"]}`, ""},
	} {
		got := schemaProviderAdapter{kimi: true}.schema([]byte(tc.schema))
		if tc.typ == "" {
			if string(got) != tc.schema {
				t.Errorf("unexpected repair %s -> %s", tc.schema, got)
			}
			continue
		}
		var object map[string]json.RawMessage
		_ = json.Unmarshal(got, &object)
		if string(object["type"]) != `"`+tc.typ+`"` {
			t.Errorf("wrong type: %s -> %s", tc.schema, got)
		}
	}
}

func TestSchemaTraversalDistinguishesSchemasFromInstanceData(t *testing.T) {
	schema := `{"properties":{"enum":{"enum":["a"]},"default":{"enum":[1]}},"$defs":{"Choice":{"enum":["a"]}},"items":[{"enum":["a"]}],"anyOf":[{"enum":["a"]}],"default":{"enum":["a"]},"examples":[{"enum":["a"]}],"enum":[{"enum":["a"]}],"x-extension":{"enum":["a"]}}`
	got := string(schemaProviderAdapter{kimi: true}.schema([]byte(schema)))
	if strings.Count(got, `"type":"string"`) != 4 || strings.Count(got, `"type":"integer"`) != 1 {
		t.Fatalf("wrong schema traversal: %s", got)
	}
	for _, fragment := range []string{`"default":{"enum":["a"]}`, `"examples":[{"enum":["a"]}]`, `"enum":[{"enum":["a"]}]`, `"x-extension":{"enum":["a"]}`} {
		if !strings.Contains(got, fragment) {
			t.Fatalf("instance data changed: %s", got)
		}
	}
}

func TestDeepSeekNULRegexPreservesOtherEscapes(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{`\0`, `\u0000`}, {`[^\0]`, `[^\u0000]`}, {`\\0`, `\\0`},
		{`\\\0`, `\\\u0000`}, {`\01\08\099`, `\01\08\099`},
		{`\u0000\x00\n\d\`, `\u0000\x00\n\d\`},
		{"\x00", "\x00"},
	} {
		if got := normalizeNULPattern(tc.in); got != tc.want {
			t.Errorf("%q -> %q, want %q", tc.in, got, tc.want)
		}
	}
	schema := `{"pattern":"\\0","default":{"pattern":"\\0"},"examples":[{"pattern":"\\0"}],"properties":{"pattern":{"pattern":"\\0"}}}`
	got := string(schemaProviderAdapter{deepseek: true}.schema([]byte(schema)))
	if strings.Count(got, `\\u0000`) != 2 || !strings.Contains(got, `"default":{"pattern":"\\0"}`) || !strings.Contains(got, `"examples":[{"pattern":"\\0"}]`) {
		t.Fatal(got)
	}
}

func TestProviderAdapterUsesResolvedEndpointNotModelOrBaseHint(t *testing.T) {
	for _, endpoint := range []*domain.Endpoint{
		nil, {BaseURL: "https://proxy.example/v1"}, {BaseURL: "https://api.deepseek.com.evil.example/v1"},
		{BaseURL: "https://api.moonshot.ai/v1", FullURL: "https://other.example/chat/completions"},
		{BaseURL: "https://open.bigmodel.cn/api/paas/v4", FullURL: "https://other.example/chat/completions"},
	} {
		if got := providerAdapter(endpoint, "kimi-deepseek-glm", domain.ProtocolOpenAIChat); got != nil {
			t.Fatalf("unexpected adapter on %+v", endpoint)
		}
	}
	for _, host := range []string{"api.moonshot.cn", "api.kimi.com", "api.kimi.ai", "api.deepseek.com"} {
		if providerAdapter(&domain.Endpoint{FullURL: "https://" + host + "/v1/messages"}, "other", domain.ProtocolAnthropic) == nil {
			t.Fatalf("missing adapter for %s", host)
		}
	}
}

func TestProviderRepairsBothCanonicalAndNativeRequests(t *testing.T) {
	for _, protocol := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic} {
		for _, native := range []bool{false, true} {
			base := Base{Endpoint: &domain.Endpoint{BaseURL: "https://api.moonshot.ai/v1"}, Model: "m"}
			rc := chatRC()
			rc.Tools = []domain.ToolDef{{Name: "pick", Parameters: json.RawMessage(`{"type":"object","properties":{"size":{"enum":["s","l"]}}}`)}}
			if native {
				rc.Origin = protocol
				switch protocol {
				case domain.ProtocolOpenAIChat:
					rc.Native = json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"pick"}],"tools":[{"type":"function","function":{"name":"pick","parameters":{"enum":["s","l"]}}}]}`)
				case domain.ProtocolOpenAIResponses:
					rc.Native = json.RawMessage(`{"model":"m","input":"pick","tools":[{"type":"function","name":"pick","parameters":{"enum":["s","l"]}}]}`)
				case domain.ProtocolAnthropic:
					rc.Native = json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"pick"}],"tools":[{"name":"pick","input_schema":{"enum":["s","l"]}}]}`)
				}
			}
			var body []byte
			var err error
			switch protocol {
			case domain.ProtocolOpenAIChat:
				body, _, err = NewOpenAIChat(base).buildRequest(rc)
			case domain.ProtocolOpenAIResponses:
				body, _, err = NewOpenAIResponses(base).buildRequest(rc)
			case domain.ProtocolAnthropic:
				body, _, err = NewAnthropic(base).buildRequest(rc)
			}
			if err != nil || !bytes.Contains(body, []byte(`"type":"string","enum"`)) {
				t.Fatalf("protocol=%s native=%v body=%s err=%v", protocol, native, body, err)
			}
		}
	}
}

func TestProviderAdapterErrorsCannotFallBackToOriginalRequest(t *testing.T) {
	for _, adapter := range []ProviderAdapter{schemaProviderAdapter{kimi: true}, schemaProviderAdapter{deepseek: true}, zhipuProviderAdapter{model: "glm"}} {
		if body, err := adapter.NormalizeRequest(domain.ProtocolOpenAIChat, true, []byte(`{"tools":`)); err == nil || body != nil {
			t.Fatalf("malformed request passed through: %s %v", body, err)
		}
	}
	rc := chatRC()
	rc.Origin = domain.ProtocolOpenAIChat
	rc.Native = json.RawMessage(`{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"out","schema":{}}}}`)
	caller := NewOpenAIChat(Base{Endpoint: &domain.Endpoint{BaseURL: "https://api.z.ai/api/paas/v4"}, Model: "glm"})
	if notes, err := caller.Prepare(rc); err == nil {
		t.Fatalf("provider rejection silently discarded: %v", notes)
	}
}

func TestStructuredOutputStrictAndVerbosityWireContract(t *testing.T) {
	for _, strict := range []*bool{nil, func() *bool { b := true; return &b }(), func() *bool { b := false; return &b }()} {
		rc := chatRC()
		rc.Verbosity = "low"
		rc.ResponseFormat = &domain.ResponseFormat{Schema: json.RawMessage(`{"type":"object","properties":{},"additionalProperties":false}`), Strict: strict}
		chat, _, err := NewOpenAIChat(Base{Model: "m"}).buildRequest(rc)
		if err != nil {
			t.Fatal(err)
		}
		responses, _, err := NewOpenAIResponses(Base{Model: "m"}).buildRequest(rc)
		if err != nil {
			t.Fatal(err)
		}
		for _, body := range [][]byte{chat, responses} {
			var fields map[string]any
			_ = json.Unmarshal(body, &fields)
			var format map[string]any
			if fields["response_format"] != nil {
				format = fields["response_format"].(map[string]any)["json_schema"].(map[string]any)
			} else {
				format = fields["text"].(map[string]any)["format"].(map[string]any)
			}
			if strict == nil {
				if _, exists := format["strict"]; exists {
					t.Fatal("invented strict default")
				}
			} else if format["strict"] != *strict {
				t.Fatalf("lost strict: %s", body)
			}
		}
		anthropic, notes, err := NewAnthropic(Base{Model: "m"}).buildRequest(rc)
		wantNotes := 1
		if strict != nil && *strict {
			wantNotes = 0
		}
		if err != nil || len(notes) != wantNotes {
			t.Fatalf("native Anthropic mapping: %v %v", notes, err)
		}
		var fields map[string]any
		_ = json.Unmarshal(anthropic, &fields)
		config := fields["output_config"].(map[string]any)
		if config["text"].(map[string]any)["verbosity"] != "low" || config["format"].(map[string]any)["type"] != "json_schema" {
			t.Fatal(string(anthropic))
		}
	}
}

func TestAnthropicStrictUpgradeNotesRespectSourceDefault(t *testing.T) {
	for _, origin := range []domain.Protocol{"", domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic} {
		for _, strict := range []*bool{nil, func() *bool { v := true; return &v }(), func() *bool { v := false; return &v }()} {
			for _, stream := range []bool{false, true} {
				rc := chatRC()
				rc.Origin = origin
				rc.Stream = stream
				rc.ResponseFormat = &domain.ResponseFormat{Schema: json.RawMessage(`{"type":"object","additionalProperties":false}`), Strict: strict}
				body, notes, err := NewAnthropic(Base{Model: "m"}).buildRequest(rc)
				if err != nil {
					t.Fatal(err)
				}
				upgraded := strict != nil && !*strict || strict == nil && origin != domain.ProtocolAnthropic
				if (len(notes) == 1) != upgraded || len(notes) > 1 || upgraded && !strings.Contains(notes[0], "strict=false") {
					t.Fatalf("origin=%q strict=%v stream=%v notes=%v", origin, strict, stream, notes)
				}
				if !bytes.Contains(body, []byte(`"format":{"type":"json_schema"`)) || bytes.Contains(body, []byte("Respond ONLY")) {
					t.Fatalf("native schema encoding changed: %s", body)
				}
				if rc.ResponseFormat.Strict != strict {
					t.Fatal("source strict mutated")
				}
			}
		}
	}
}
