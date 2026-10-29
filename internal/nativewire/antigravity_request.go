package nativewire

import (
	"bytes"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
)

var agWireInvalid = errors.New("unsupported or invalid Antigravity Chat request; signed/stateful reasoning and multimodal input are not supported")
var agName = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_.-]{0,63}$`)
var agModel = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)
var agProject = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._-]{0,127}$`)

type agFunction struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}
type agCall struct {
	ID       string     `json:"id"`
	Type     string     `json:"type"`
	Function agFunction `json:"function"`
}
type agMessage struct {
	Role       string          `json:"role"`
	Content    json.RawMessage `json:"content"`
	ToolCalls  []agCall        `json:"tool_calls"`
	ToolCallID string          `json:"tool_call_id"`
}
type agTool struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description"`
		Parameters  map[string]any `json:"parameters"`
		Strict      *bool          `json:"strict"`
	} `json:"function"`
}
type agChat struct {
	Model         string      `json:"model"`
	Messages      []agMessage `json:"messages"`
	Stream        bool        `json:"stream"`
	StreamOptions *struct {
		IncludeUsage bool `json:"include_usage"`
	} `json:"stream_options"`
	Tools               []agTool        `json:"tools"`
	ToolChoice          json.RawMessage `json:"tool_choice"`
	Temperature         *float64        `json:"temperature"`
	TopP                *float64        `json:"top_p"`
	MaxTokens           *int            `json:"max_tokens"`
	MaxCompletionTokens *int            `json:"max_completion_tokens"`
	Stop                json.RawMessage `json:"stop"`
	N                   *int            `json:"n"`
}

func agDecode(b []byte, v any) error {
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if d.Decode(v) != nil {
		return agWireInvalid
	}
	var extra any
	if d.Decode(&extra) != io.EOF {
		return agWireInvalid
	}
	return nil
}
func agID() string {
	b := make([]byte, 16)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}
func agContent(raw json.RawMessage) ([]map[string]any, error) {
	if len(raw) == 0 || string(raw) == "null" {
		return nil, nil
	}
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return []map[string]any{{"text": text}}, nil
	}
	var parts []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if agDecode(raw, &parts) != nil {
		return nil, agWireInvalid
	}
	out := []map[string]any{}
	for _, p := range parts {
		if p.Type != "text" {
			return nil, agWireInvalid
		}
		out = append(out, map[string]any{"text": p.Text})
	}
	return out, nil
}
func agEncodeRequest(raw []byte, project string) (agChat, []byte, error) {
	var chat agChat
	if agDecode(raw, &chat) != nil || !agProject.MatchString(project) || !agModel.MatchString(chat.Model) || len(chat.Messages) == 0 || chat.N != nil && *chat.N != 1 {
		return chat, nil, agWireInvalid
	}
	request := map[string]any{}
	contents := []map[string]any{}
	system := []map[string]any{}
	pending := map[string]string{}
	used := map[string]bool{}
	for _, m := range chat.Messages {
		parts, err := agContent(m.Content)
		if err != nil {
			return chat, nil, err
		}
		if m.Role != "assistant" && len(m.ToolCalls) > 0 || m.Role != "tool" && m.ToolCallID != "" {
			return chat, nil, agWireInvalid
		}
		role := m.Role
		switch role {
		case "system", "developer":
			if len(contents) > 0 || len(m.ToolCalls) > 0 {
				return chat, nil, agWireInvalid
			}
			system = append(system, parts...)
			continue
		case "assistant":
			if len(pending) > 0 {
				return chat, nil, agWireInvalid
			}
			role = "model"
			for _, call := range m.ToolCalls {
				if !agName.MatchString(call.Function.Name) || !agModel.MatchString(call.ID) || call.Type != "function" || used[call.ID] {
					return chat, nil, agWireInvalid
				}
				var args map[string]any
				if json.Unmarshal([]byte(call.Function.Arguments), &args) != nil || args == nil {
					return chat, nil, agWireInvalid
				}
				used[call.ID] = true
				pending[call.ID] = call.Function.Name
				parts = append(parts, map[string]any{"functionCall": map[string]any{"id": call.ID, "name": call.Function.Name, "args": args}})
			}
		case "tool":
			name, ok := pending[m.ToolCallID]
			if !ok || len(parts) != 1 {
				return chat, nil, agWireInvalid
			}
			delete(pending, m.ToolCallID)
			parts = []map[string]any{{"functionResponse": map[string]any{"id": m.ToolCallID, "name": name, "response": map[string]any{"result": parts[0]["text"]}}}}
			role = "user"
		case "user":
			if len(pending) > 0 {
				return chat, nil, agWireInvalid
			}
		default:
			return chat, nil, agWireInvalid
		}
		if len(parts) == 0 {
			return chat, nil, agWireInvalid
		}
		// Gemini groups adjacent tool results into one user turn.
		if m.Role == "tool" && len(contents) > 0 && contents[len(contents)-1]["role"] == "user" {
			last := contents[len(contents)-1]
			last["parts"] = append(last["parts"].([]map[string]any), parts...)
		} else {
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
	}
	if len(contents) == 0 || len(pending) > 0 {
		return chat, nil, agWireInvalid
	}
	request["contents"] = contents
	if len(system) > 0 {
		request["systemInstruction"] = map[string]any{"parts": system}
	}
	gen := map[string]any{}
	if chat.Temperature != nil {
		if *chat.Temperature < 0 || *chat.Temperature > 2 {
			return chat, nil, agWireInvalid
		}
		gen["temperature"] = *chat.Temperature
	}
	if chat.TopP != nil {
		if *chat.TopP < 0 || *chat.TopP > 1 {
			return chat, nil, agWireInvalid
		}
		gen["topP"] = *chat.TopP
	}
	max := chat.MaxTokens
	if chat.MaxCompletionTokens != nil {
		if max != nil {
			return chat, nil, agWireInvalid
		}
		max = chat.MaxCompletionTokens
	}
	if max != nil {
		if *max < 1 {
			return chat, nil, agWireInvalid
		}
		gen["maxOutputTokens"] = *max
	}
	if len(chat.Stop) > 0 && string(chat.Stop) != "null" {
		var stop []string
		var s string
		if json.Unmarshal(chat.Stop, &s) == nil {
			stop = []string{s}
		} else if json.Unmarshal(chat.Stop, &stop) != nil {
			return chat, nil, agWireInvalid
		}
		if len(stop) > 5 {
			return chat, nil, agWireInvalid
		}
		gen["stopSequences"] = stop
	}
	if len(gen) > 0 {
		request["generationConfig"] = gen
	}
	declarations := []map[string]any{}
	names := map[string]bool{}
	for _, t := range chat.Tools {
		f := t.Function
		if t.Type != "function" || !agName.MatchString(f.Name) || names[f.Name] || f.Strict != nil && *f.Strict {
			return chat, nil, agWireInvalid
		}
		names[f.Name] = true
		schema := f.Parameters
		if schema == nil {
			schema = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		declarations = append(declarations, map[string]any{"name": f.Name, "description": f.Description, "parametersJsonSchema": schema})
	}
	if len(declarations) > 0 {
		request["tools"] = []any{map[string]any{"functionDeclarations": declarations}}
	}
	if len(chat.ToolChoice) > 0 && string(chat.ToolChoice) != "null" {
		mode := ""
		allowed := []string{}
		var choice string
		if json.Unmarshal(chat.ToolChoice, &choice) == nil {
			switch choice {
			case "auto":
				mode = "AUTO"
			case "none":
				mode = "NONE"
			case "required":
				mode = "ANY"
			default:
				return chat, nil, agWireInvalid
			}
		} else {
			var c struct {
				Type     string `json:"type"`
				Function struct {
					Name string `json:"name"`
				} `json:"function"`
			}
			if agDecode(chat.ToolChoice, &c) != nil || c.Type != "function" || !names[c.Function.Name] {
				return chat, nil, agWireInvalid
			}
			mode = "ANY"
			allowed = append(allowed, c.Function.Name)
		}
		if mode != "NONE" && len(declarations) == 0 {
			return chat, nil, agWireInvalid
		}
		fc := map[string]any{"mode": mode}
		if len(allowed) > 0 {
			fc["allowedFunctionNames"] = allowed
		}
		request["toolConfig"] = map[string]any{"functionCallingConfig": fc}
	}
	// A new opaque request/session identity is generated, never a device fingerprint.
	sessionBytes := make([]byte, 8)
	if _, err := rand.Read(sessionBytes); err != nil {
		return chat, nil, err
	}
	request["sessionId"] = "-" + strconv.FormatUint(binary.BigEndian.Uint64(sessionBytes)&((1<<63)-1), 10)
	envelope := map[string]any{"model": chat.Model, "project": project, "userAgent": "antigravity", "requestType": "agent", "requestId": "agent-" + agID(), "request": request}
	b, err := json.Marshal(envelope)
	return chat, b, err
}

// ValidateAntigravityChat checks the same conversion contract used by the native
// transport, without dispatching a request or resolving account credentials.
// Preparers must call it before claiming exact Chat compatibility.
func ValidateAntigravityChat(payload []byte) error {
	_, _, err := agEncodeRequest(payload, "validation-project")
	return err
}
