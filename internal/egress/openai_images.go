package egress

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"strings"

	"ferry/internal/domain"
)

const maxImageResponseBody = 64 << 20

// OpenAIImages adapts /v1/images/generations and /v1/images/edits. Images is
// deliberately native-only: Ferry preserves the provider-specific body and
// response instead of attempting a text-protocol conversion.
type OpenAIImages struct {
	Base
	classifier responseFailureClassifier
}

func NewOpenAIImages(b Base) *OpenAIImages { return &OpenAIImages{Base: b} }

// NewCodexImages builds the Codex OAuth variant of the native Images adapter.
// It uses bearer-only credential placement, the same Codex identity envelope
// as text calls, and Codex-specific quota/auth failure classification.
func NewCodexImages(b Base, accountID string) *OpenAIImages {
	b.Credential.Header = "Authorization"
	b.Credential.QueryParam = ""
	b.Credential.Prefix = "Bearer "
	b.ExtraHeaders = codexProviderHeaders(b.ExtraHeaders, accountID, b.CodexClientVersion)
	return &OpenAIImages{Base: b, classifier: classifyCodexHTTPFailure}
}

func (o *OpenAIImages) buildRequest(rc *domain.RequestContract) ([]byte, string, error) {
	if rc == nil || rc.Protocol != domain.ProtocolOpenAIImages {
		return nil, "", fmt.Errorf("openai-images requires an openai-images request")
	}
	operation := rc.Operation
	if operation == "" {
		operation = "generation"
	}
	switch operation {
	case "generation":
		if body := imageJSONPassthrough(rc, o.Model); body != nil {
			return body, "application/json", nil
		}
		prompt := imagePrompt(rc)
		if prompt == "" {
			prompt = "A simple neutral test image."
		}
		body, err := json.Marshal(map[string]any{"model": o.Model, "prompt": prompt, "n": 1})
		return body, "application/json", err
	case "edit":
		return rewriteImageEdit(rc, o.Model)
	default:
		return nil, "", fmt.Errorf("unsupported openai-images operation %q", operation)
	}
}

func imageJSONPassthrough(rc *domain.RequestContract, upstreamModel string) []byte {
	if rc.Origin != domain.ProtocolOpenAIImages || len(rc.Native) == 0 || upstreamModel == "" || rc.Operation == "edit" {
		return nil
	}
	var body map[string]json.RawMessage
	if json.Unmarshal(rc.Native, &body) != nil {
		return nil
	}
	if _, ok := body["model"]; !ok {
		return nil
	}
	body["model"], _ = json.Marshal(upstreamModel)
	out, err := json.Marshal(body)
	if err != nil {
		return nil
	}
	return out
}

func imagePrompt(rc *domain.RequestContract) string {
	for _, message := range rc.Messages {
		for _, block := range message.Blocks {
			if block.Kind == domain.BlockText && strings.TrimSpace(block.Text) != "" {
				return block.Text
			}
		}
	}
	return ""
}

func rewriteImageEdit(rc *domain.RequestContract, upstreamModel string) ([]byte, string, error) {
	if rc.Origin != domain.ProtocolOpenAIImages || len(rc.Native) == 0 {
		return nil, "", fmt.Errorf("image edits require a native multipart request")
	}
	mediaType, params, err := mime.ParseMediaType(rc.NativeContentType)
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") || params["boundary"] == "" {
		return nil, "", fmt.Errorf("image edits require multipart/form-data with a boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(rc.Native), params["boundary"])
	var out bytes.Buffer
	writer := multipart.NewWriter(&out)
	modelSeen := false
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, "", fmt.Errorf("read image edit part: %w", nextErr)
		}
		header := cloneMIMEHeader(part.Header)
		dst, createErr := writer.CreatePart(header)
		if createErr != nil {
			part.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return nil, "", fmt.Errorf("copy image edit part: %w", createErr)
		}
		if part.FormName() == "model" {
			modelSeen = true
			_, err = io.WriteString(dst, upstreamModel)
		} else {
			_, err = io.Copy(dst, part)
		}
		part.Close() // #nosec G104 -- the copy error is checked on the next line and the enclosing multipart writer Close is verified
		if err != nil {
			return nil, "", fmt.Errorf("copy image edit body: %w", err)
		}
	}
	if !modelSeen {
		field, fieldErr := writer.CreateFormField("model")
		if fieldErr != nil {
			return nil, "", fieldErr
		}
		_, _ = io.WriteString(field, upstreamModel)
	}
	if err := writer.Close(); err != nil {
		return nil, "", fmt.Errorf("finish image edit body: %w", err)
	}
	return out.Bytes(), writer.FormDataContentType(), nil
}

func cloneMIMEHeader(header textproto.MIMEHeader) textproto.MIMEHeader {
	clone := make(textproto.MIMEHeader, len(header))
	for key, values := range header {
		clone[key] = append([]string(nil), values...)
	}
	return clone
}

func (o *OpenAIImages) Invoke(ctx context.Context, rc *domain.RequestContract) (*domain.Response, *domain.Failure) {
	payload, contentType, err := o.buildRequest(rc)
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailUnsupportedParam, Message: "build image request: " + err.Error()}
	}
	path := "/images/generations"
	if rc.Operation == "edit" {
		path = "/images/edits"
	}
	resp, failure := o.doSendClassified(ctx, path, payload,
		map[string]string{"Content-Type": contentType, "Accept": "application/json"}, o.classifier)
	if failure != nil {
		return nil, failure
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxImageResponseBody+1))
	if err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "read image response: " + err.Error()}
	}
	if len(body) > maxImageResponseBody {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "image response exceeds 64 MiB"}
	}
	result := &domain.Response{Model: o.Model, FinishReason: "stop", NativeBody: body, NativeContentType: resp.Header.Get("Content-Type")}
	var envelope struct {
		Model string          `json:"model"`
		Usage json.RawMessage `json:"usage"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, &domain.Failure{Type: domain.FailBadResponse, Message: "decode image response: " + err.Error()}
	}
	if envelope.Model != "" {
		result.Model = envelope.Model
	}
	result.Usage = decodeUsage(envelope.Usage)
	return result, nil
}

func (o *OpenAIImages) Stream(context.Context, *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	return nil, nil, &domain.Failure{Type: domain.FailUnsupportedParam, Message: "openai-images does not support streaming"}
}

var _ Caller = (*OpenAIImages)(nil)
var _ Preparer = (*OpenAIImages)(nil)
