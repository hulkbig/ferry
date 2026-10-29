package ingress

import (
	"strings"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

// TestRealisticAgentTurnsRemainCrossProtocolCompatible exercises the shared
// agent surface as complete multi-turn requests instead of isolated fields.
// It guards the ingress conversion markers and every egress preflight because
// either layer can otherwise exclude all candidates before the first attempt.
func TestRealisticAgentTurnsRemainCrossProtocolCompatible(t *testing.T) {
	fixtures := []struct {
		name  string
		parse func() (*domain.RequestContract, error)
	}{
		{
			name: "openai-chat Hermes tool continuation",
			parse: func() (*domain.RequestContract, error) {
				return (&OpenAIChatHandler{}).toContract([]byte(`{
					"model":"hermes-agent",
					"messages":[
						{"role":"system","content":"Be useful."},
						{"role":"user","content":"Find the prior session."},
						{"role":"assistant","content":null,"tool_calls":[{"id":"call-1","type":"function","function":{"name":"session_search","arguments":"{\"query\":\"prior\"}"}}]},
						{"role":"tool","name":"session_search","tool_call_id":"call-1","content":"found"}
					],
					"tools":[{"type":"function","function":{"name":"session_search","description":"Search sessions","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}}],
					"tool_choice":"auto",
					"parallel_tool_calls":true,
					"reasoning_effort":"high",
					"max_tokens":65536,
					"temperature":0.4,
					"top_p":0.9,
					"stream":true,
					"stream_options":{"include_usage":true}
				}`))
			},
		},
		{
			name: "openai-responses function continuation",
			parse: func() (*domain.RequestContract, error) {
				return (&ResponsesHandler{}).toContract([]byte(`{
					"model":"agent",
					"instructions":"Be useful.",
					"input":[
						{"type":"message","id":"msg-1","status":"completed","role":"user","content":[{"type":"input_text","text":"Find the prior session."}]},
						{"type":"function_call","id":"fc-1","status":"completed","call_id":"call-1","name":"session_search","arguments":"{\"query\":\"prior\"}"},
						{"type":"function_call_output","id":"fco-1","status":"completed","call_id":"call-1","output":"found"}
					],
					"tools":[{"type":"function","name":"session_search","description":"Search sessions","parameters":{"type":"object","properties":{"query":{"type":"string"}}}}],
					"tool_choice":"auto",
					"parallel_tool_calls":true,
					"reasoning":{"effort":"high"},
					"max_output_tokens":65536,
					"temperature":0.4,
					"top_p":0.9,
					"stream":true
				}`))
			},
		},
		{
			name: "anthropic tool continuation",
			parse: func() (*domain.RequestContract, error) {
				return (&AnthropicHandler{}).toContract([]byte(`{
					"model":"agent",
					"system":"Be useful.",
					"messages":[
						{"role":"user","content":"Find the prior session."},
						{"role":"assistant","content":[{"type":"tool_use","id":"call-1","name":"session_search","input":{"query":"prior"}}]},
						{"role":"user","content":[{"type":"tool_result","tool_use_id":"call-1","content":"found"},{"type":"text","text":"Use that result."}]}
					],
					"tools":[{"name":"session_search","description":"Search sessions","input_schema":{"type":"object","properties":{"query":{"type":"string"}}}}],
					"tool_choice":{"type":"auto","disable_parallel_tool_use":false},
					"thinking":{"type":"enabled","budget_tokens":4096},
					"max_tokens":65536,
					"temperature":0.4,
					"top_p":0.9,
					"stream":true
				}`))
			},
		},
	}

	adapters := []struct {
		name    string
		prepare func(*domain.RequestContract) ([]string, error)
	}{
		{name: "openai-chat", prepare: egress.NewOpenAIChat(egress.Base{Model: "upstream"}).Prepare},
		{name: "openai-responses", prepare: egress.NewOpenAIResponses(egress.Base{Model: "upstream"}).Prepare},
		{name: "anthropic", prepare: egress.NewAnthropic(egress.Base{Model: "upstream"}).Prepare},
	}

	for _, fixture := range fixtures {
		t.Run(fixture.name, func(t *testing.T) {
			rc, err := fixture.parse()
			if err != nil {
				t.Fatalf("parse: %v", err)
			}
			if len(rc.ConversionIssues) != 0 {
				t.Fatalf("shared agent request was marked native-only: %v", rc.ConversionIssues)
			}
			for _, adapter := range adapters {
				t.Run(adapter.name, func(t *testing.T) {
					notes, err := adapter.prepare(rc)
					if err != nil || len(notes) != 0 {
						t.Fatalf("candidate would be excluded before attempt: notes=%v err=%v", notes, err)
					}
				})
			}
		})
	}
}

func TestCodexResponsesOptionalFieldsUseBestEffortCompatibility(t *testing.T) {
	rc, err := (&ResponsesHandler{}).toContract([]byte(`{
		"model":"gpt-6-sol",
		"input":"hello",
		"stream":true,
		"stream_options":{"reasoning_summary_delivery":"auto"},
		"include":["reasoning.encrypted_content"]
	}`))
	if err != nil {
		t.Fatal(err)
	}
	caller := egress.NewCodexCaller(egress.Base{Model: "gpt-6-sol"}, "account")
	notes, err := caller.Prepare(rc)
	if err != nil {
		t.Fatalf("Codex request preparation failed: %v", err)
	}
	joined := strings.Join(append(notes, rc.MappingNotes...), "; ")
	if !strings.Contains(joined, "stream_options") || !strings.Contains(joined, "include extras omitted") {
		t.Fatalf("optional field losses not reported: %v", notes)
	}
	if !domain.DegradationAllowed(false, domain.CompatDegraded) {
		t.Fatal("default best-effort policy rejected optional Responses field omissions")
	}
}
