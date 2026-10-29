package credentialimport

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSub2APIExportAndRedaction(t *testing.T) {
	r := Request{Source: "sub2api", Namespace: "office", Document: `{"version":1,"accounts":[{"name":"OpenAI","platform":"openai","type":"oauth","credentials":{"access_token":"access-private","refresh_token":"refresh-private","chatgpt_account_id":"account-1","expires_at":1900000000}},{"name":"Claude","platform":"anthropic","type":"oauth","credentials":{"access_token":"claude-private","refresh_token":"claude-refresh"}},{"name":"Gateway","platform":"openai","type":"apikey","credentials":{"api_key":"key-private","base_url":"https://example.test/v1"}},{"name":"Gemini","platform":"gemini","type":"oauth","credentials":{"access_token":"unsupported-private"}}]}`}
	p, e := Parse(r)
	if e != nil {
		t.Fatal(e)
	}
	if len(p.Items) != 4 {
		t.Fatal("missing items")
	}
	for _, i := range p.Items[:3] {
		if i.Error != "" {
			t.Fatal(i.Error)
		}
	}
	if p.Items[3].Error == "" {
		t.Fatal("unsupported account accepted")
	}
	var normalized map[string]any
	_ = json.Unmarshal(p.Items[0].Auth, &normalized)
	if normalized["account_id"] != "account-1" || normalized["expired"] == nil {
		t.Fatal("lost account identity or expiry")
	}
	raw, _ := json.Marshal(p)
	for _, secret := range []string{"access-private", "refresh-private", "claude-private", "claude-refresh", "key-private", "unsupported-private"} {
		if strings.Contains(string(raw), secret) {
			t.Fatal("preview leaked credential")
		}
	}
	p2, _ := Parse(r)
	if p.Items[0].ID != p2.Items[0].ID {
		t.Fatal("unstable identity")
	}
}
func TestCLIProxyLegacyAndV8(t *testing.T) {
	for _, document := range []string{
		`api-keys: [client-secret]
openai-compatibility:
 - name: gateway
   base-url: https://example.test/v1
   api-key-entries:
    - api-key: upstream-secret
   models:
    - name: model-a
      alias: alias-a
claude-api-key:
 - api-key: claude-secret
`,
		`access:
 api-keys: [client-secret]
api-keys:
 openai-compatibility:
  - name: gateway
    base-url: https://example.test/v1
    keys:
     - api-key: upstream-secret
    models:
     - name: model-a
       alias: alias-a
 claude:
  - keys:
     - api-key: claude-secret
`} {
		p, e := Parse(Request{Source: "cliproxyapi", Namespace: "office", Document: document})
		if e != nil {
			t.Fatal(e)
		}
		if len(p.Items) != 3 {
			t.Fatalf("got %d items", len(p.Items))
		}
		for _, i := range p.Items {
			if i.Error != "" {
				t.Fatal(i.Error)
			}
		}
		if p.Items[1].Secret != "upstream-secret" || len(p.Items[1].Models) != 1 || p.Items[1].Models[0] != "model-a" {
			t.Fatal("lost upstream key/models")
		}
	}
}
func TestRejectUnsafeAndUnsupportedUploads(t *testing.T) {
	for _, document := range []string{`{"type":"codex","use_siwc":true,"access_token":"access","refresh_token":"refresh"}`, `{"type":"codex","client_id":"foreign","access_token":"access","refresh_token":"refresh"}`, `{"type":"claude","access_token":"***","refresh_token":"refresh"}`, `{"type":"antigravity","access_token":"access"}`} {
		p, e := Parse(Request{Source: "cliproxyapi", Namespace: "office", AuthFiles: []json.RawMessage{json.RawMessage(document)}})
		if e != nil {
			t.Fatal(e)
		}
		if p.Items[0].Error == "" {
			t.Fatal("unsafe credential accepted")
		}
	}
	for _, base := range []string{"https://user:secret@example.test/v1", "https://example.test?key=secret", "file:///tmp/a"} {
		document := `{"accounts":[{"platform":"openai","type":"apikey","credentials":{"api_key":"private","base_url":"` + base + `"}}]}`
		p, e := Parse(Request{Source: "sub2api", Namespace: "office", Document: document})
		if e != nil {
			t.Fatal(e)
		}
		if p.Items[0].Error == "" || p.Items[0].BaseURL != "" {
			t.Fatal("unsafe endpoint accepted")
		}
	}
	for _, r := range []Request{{Source: "bad", Namespace: "ok"}, {Source: "sub2api", Namespace: "../bad"}, {Source: "sub2api", Namespace: "ok", Document: "{"}, {Source: "sub2api", Namespace: "ok", Document: `{"version":2,"accounts":[]}`}, {Source: "sub2api", Namespace: "ok", Document: strings.Repeat("x", MaxBytes+1)}} {
		if _, e := Parse(r); e == nil {
			t.Fatal("invalid request accepted")
		}
	}
}

func TestUnknownV8GroupAndInvalidModelsRemainVisible(t *testing.T) {
	for _, document := range []string{`api-keys:
 qwen:
  - keys:
     - api-key: private
`, `codex-api-key:
 - api-key: private
`, `openai-compatibility:
 - base-url: https://example.test/v1
   api-key-entries:
    - api-key: private
   models:
    - name: "bad model"
`} {
		p, e := Parse(Request{Source: "cliproxyapi", Namespace: "office", Document: document})
		if e != nil {
			t.Fatal(e)
		}
		if len(p.Items) != 1 || p.Items[0].Error == "" {
			t.Fatal("invalid item silently accepted or dropped")
		}
	}
}

func TestSourceEndpointAndAuthSemantics(t *testing.T) {
	for _, tc := range []struct{ source, platform, base, want, header string }{
		{"cliproxyapi", "claude", "https://gateway.test/prefix", "https://gateway.test/prefix/v1", "Authorization"},
		{"cliproxyapi", "claude", "https://api.anthropic.com", "https://api.anthropic.com/v1", "x-api-key"},
		{"sub2api", "anthropic", "https://gateway.test", "https://gateway.test/v1", "x-api-key"},
		{"sub2api", "openai", "https://gateway.test", "https://gateway.test/v1", "Authorization"},
		{"sub2api", "openai", "https://gateway.test/api/v4", "https://gateway.test/api/v4", "Authorization"},
		{"sub2api", "openai", "https://gateway.test/api/v1/chat/completions", "https://gateway.test/api/v1", "Authorization"},
		{"cliproxyapi", "openai-compatibility", "https://gateway.test/api/v1", "https://gateway.test/api/v1", "Authorization"},
	} {
		item := apiKey(map[string]any{"api_key": "private", "base_url": tc.base}, tc.platform, tc.source)
		if item.Error != "" || item.BaseURL != tc.want || item.Header != tc.header {
			t.Fatalf("%s %s: got %s %s (%s)", tc.source, tc.platform, item.BaseURL, item.Header, item.Error)
		}
	}
}
