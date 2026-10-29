package ingress

import (
	"encoding/json"
	"strings"
	"testing"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

func TestAnthropicStructuredOutputContract(t *testing.T) {
	schema := `{"type":"object","properties":{"ok":{"type":"boolean"}},"required":["ok"],"additionalProperties":false}`
	for _, stream := range []string{"false", "true"} {
		body := []byte(`{"model":"claude","max_tokens":100,"stream":` + stream + `,"messages":[{"role":"user","content":"test"}],"output_config":{"text":{"verbosity":"low"},"format":{"type":"json_schema","schema":` + schema + `}}}`)
		rc, err := (&AnthropicHandler{}).toContract(body)
		if err != nil {
			t.Fatal(err)
		}
		if rc.ResponseFormat == nil || string(rc.ResponseFormat.Schema) != schema || rc.ResponseFormat.Strict == nil || !*rc.ResponseFormat.Strict || rc.Verbosity != "low" || len(rc.ConversionIssues) != 0 {
			t.Fatalf("contract=%+v", rc)
		}
		if !rc.ExtractRequirements().StructuredOutput {
			t.Fatal("structured output missing from capability requirements")
		}
		for _, caller := range []egress.Preparer{egress.NewOpenAIChat(egress.Base{Model: "m"}), egress.NewOpenAIResponses(egress.Base{Model: "m"})} {
			if notes, err := caller.Prepare(rc); err != nil || len(notes) != 0 {
				t.Fatalf("native format could not map: notes=%v err=%v", notes, err)
			}
		}
	}
}

func TestAnthropicStructuredOutputRejectsMalformedSchema(t *testing.T) {
	for _, format := range []string{`{"type":"json_schema"}`, `{"type":"json_schema","schema":null}`, `{"type":"json_schema","schema":[]}`, `{"type":"json_schema","schema":"bad"}`} {
		_, err := (&AnthropicHandler{}).toContract([]byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"output_config":{"format":` + format + `}}`))
		if err == nil || !strings.Contains(err.Error(), "schema must be a JSON object") {
			t.Fatalf("format=%s err=%v", format, err)
		}
	}
}

func TestAnthropicUnknownOutputConfigCannotDisappearAcrossProtocols(t *testing.T) {
	for _, config := range []string{`{"format":{"type":"future","schema":{}}}`, `{"format":{"type":"json_schema","schema":{},"future":true}}`, `{"effort":"high"}`, `{"text":{"future":true}}`} {
		body := []byte(`{"model":"m","messages":[{"role":"user","content":"x"}],"output_config":` + config + `}`)
		rc, err := (&AnthropicHandler{}).toContract(body)
		if err != nil {
			t.Fatal(err)
		}
		if len(rc.ConversionIssues) == 0 {
			t.Fatalf("unknown config lost: %s", config)
		}
		if _, err := egress.NewAnthropic(egress.Base{Model: "m"}).Prepare(rc); err != nil {
			t.Fatalf("same-protocol rejected: %v", err)
		}
		if _, err := egress.NewOpenAIChat(egress.Base{Model: "m"}).Prepare(rc); err == nil {
			t.Fatal("cross-protocol silently dropped native field")
		}
	}
}

func TestOpenAIStrictStructuredOutputCanMapToAnthropic(t *testing.T) {
	for _, strict := range []string{"true", "false", "omitted"} {
		schema := `{"type":"object","properties":{},"additionalProperties":false}`
		inputs := []struct {
			handler interface {
				toContract([]byte) (*domain.RequestContract, error)
			}
			body string
		}{
			{&OpenAIChatHandler{}, `{"model":"m","messages":[{"role":"user","content":"x"}],"response_format":{"type":"json_schema","json_schema":{"name":"out","strict":` + strict + `,"schema":` + schema + `}}}`},
			{&ResponsesHandler{}, `{"model":"m","input":"x","text":{"format":{"type":"json_schema","name":"out","strict":` + strict + `,"schema":` + schema + `}}}`},
		}
		for _, input := range inputs {
			body := input.body
			if strict == "omitted" {
				body = strings.ReplaceAll(body, `"strict":omitted,`, "")
			}
			rc, err := input.handler.toContract([]byte(body))
			if err != nil {
				t.Fatal(err)
			}
			if rc.ResponseFormat == nil || !json.Valid(rc.ResponseFormat.Schema) ||
				(strict == "omitted" && rc.ResponseFormat.Strict != nil) ||
				(strict != "omitted" && (rc.ResponseFormat.Strict == nil || *rc.ResponseFormat.Strict != (strict == "true"))) {
				t.Fatalf("strict lost: %+v", rc.ResponseFormat)
			}
			wantNotes := 1
			if strict == "true" {
				wantNotes = 0
			}
			if notes, err := egress.NewAnthropic(egress.Base{Model: "m"}).Prepare(rc); err != nil || len(notes) != wantNotes || (wantNotes == 1 && !strings.Contains(notes[0], "strict=false")) {
				t.Fatalf("strict mapping notes=%v err=%v", notes, err)
			}
		}
	}
}
