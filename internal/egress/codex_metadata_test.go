package egress

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"ferry/internal/domain"
)

const codexDiagnosticEvent = `{"type":"response.metadata","response_id":"resp-diagnostic","sequence_number":1,"metadata":{"openai_chatgpt_moderation_metadata":{"prompt":{"omnimod":{"outputs":[{"is_blocked":false}]}},"generation":{}}}}`

func metadataRequest(origin domain.Protocol) *domain.RequestContract {
	rc := &domain.RequestContract{Origin: origin, Protocol: origin, Model: "test-model",
		Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: "hello"}}}}}
	if origin == domain.ProtocolOpenAIResponses {
		rc.Native = json.RawMessage(`{"model":"test-model","input":"hello"}`)
	}
	return rc
}

func TestCodexDiagnosticsDoNotRejectGeneratedResponses(t *testing.T) {
	for _, origin := range []domain.Protocol{domain.ProtocolOpenAIResponses, domain.ProtocolOpenAIChat, domain.ProtocolAnthropic} {
		t.Run(string(origin), func(t *testing.T) {
			capture := newCodexCapture("data: " + codexDiagnosticEvent + "\n\n" + codexDoneSSE)
			defer capture.close()
			caller := NewCodexCaller(codexBase(t, capture), "")
			response, failure := caller.Invoke(context.Background(), metadataRequest(origin))
			if failure != nil || len(response.ConversionIssues) != 0 || len(response.ValidationIssues) != 0 {
				t.Fatalf("diagnostics rejected: response=%+v failure=%+v", response, failure)
			}
			if len(response.Content) != 1 || response.Content[0].Text != "hi" || response.Usage.OutputTokens != 2 || len(response.Warnings) != 1 {
				t.Fatalf("content/usage/diagnostic warning lost: %+v", response)
			}
			events, cancel, failure := caller.Stream(context.Background(), metadataRequest(origin))
			mustNoFail(t, failure)
			defer cancel()
			var text string
			var diagnostic, ended bool
			for _, event := range drain(t, events) {
				if event.Err != nil {
					t.Fatal(event.Err)
				}
				text += event.TextDelta
				ended = ended || event.Kind == domain.EvEnd
				if event.NativeFrame != nil && event.NativeFrame.Data == codexDiagnosticEvent {
					diagnostic = true
				}
			}
			if text != "hi" || !ended || diagnostic != (origin == domain.ProtocolOpenAIResponses) {
				t.Fatalf("stream text=%q ended=%v native diagnostic=%v", text, ended, diagnostic)
			}
		})
	}
}

func TestCodexMetadataDoesNotHideUnknownContentOrTerminalFailures(t *testing.T) {
	unknown := `{"type":"response.metadata","metadata":{"future_output":{"text":"must not disappear"}}}`
	for _, bad := range []string{unknown, `{"type":"response.metadata","metadata":null}`,
		strings.Replace(codexDiagnosticEvent, `"sequence_number":1`, `"delta":"unmapped output"`, 1)} {
		capture := newCodexCapture("data: " + bad + "\n\n" + codexDoneSSE)
		caller := NewCodexCaller(codexBase(t, capture), "")
		response, failure := caller.Invoke(context.Background(), metadataRequest(domain.ProtocolOpenAIChat))
		if failure == nil && len(response.ConversionIssues) == 0 {
			t.Fatal("unknown metadata silently discarded")
		}
		events, cancel, failure := caller.Stream(context.Background(), metadataRequest(domain.ProtocolOpenAIChat))
		mustNoFail(t, failure)
		var rejected bool
		for _, event := range drain(t, events) {
			rejected = rejected || event.Err != nil
		}
		cancel()
		capture.close()
		if !rejected {
			t.Fatal("unknown metadata stream silently discarded")
		}
	}
	for _, terminal := range []string{
		`{"type":"response.failed","response":{"status":"failed","error":{"code":"content_filter","message":"blocked"}}}`,
		`{"type":"response.completed","response":{"status":"in_progress"}}`,
	} {
		capture := newCodexCapture("data: " + codexDiagnosticEvent + "\n\ndata: " + terminal + "\n\n")
		caller := NewCodexCaller(codexBase(t, capture), "")
		response, failure := caller.Invoke(context.Background(), metadataRequest(domain.ProtocolOpenAIResponses))
		capture.close()
		if failure == nil && len(response.ValidationIssues) == 0 {
			t.Fatal("diagnostics hid a failed or malformed terminal response")
		}
	}
}
