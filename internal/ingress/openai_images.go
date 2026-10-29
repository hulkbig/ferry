package ingress

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"mime/multipart"
	"net/http"
	"strings"

	"ferry/internal/domain"
)

// OpenAIImagesHandler serves POST /v1/images/generations and
// POST /v1/images/edits.
type OpenAIImagesHandler struct{ Eng Engine }

func (h *OpenAIImagesHandler) Protocol() domain.Protocol { return domain.ProtocolOpenAIImages }

func (h *OpenAIImagesHandler) toGenerationContract(body []byte) (*domain.RequestContract, error) {
	var request struct {
		Model  string `json:"model"`
		Prompt string `json:"prompt"`
	}
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, perrf("invalid JSON body: %v", err)
	}
	if strings.TrimSpace(request.Model) == "" {
		return nil, perrf("model is required")
	}
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Origin: domain.ProtocolOpenAIImages,
		Model: request.Model, Native: json.RawMessage(body),
		NativeContentType: "application/json", Operation: "generation",
	}
	if strings.TrimSpace(request.Prompt) != "" {
		rc.Messages = []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: request.Prompt}}}}
	}
	return rc, nil
}

func (h *OpenAIImagesHandler) toEditContract(body []byte, contentType string) (*domain.RequestContract, error) {
	mediaType, params, err := mime.ParseMediaType(contentType)
	if err != nil || !strings.EqualFold(mediaType, "multipart/form-data") || params["boundary"] == "" {
		return nil, perrf("Content-Type must be multipart/form-data with a boundary")
	}
	reader := multipart.NewReader(bytes.NewReader(body), params["boundary"])
	model, prompt := "", ""
	for {
		part, nextErr := reader.NextPart()
		if nextErr == io.EOF {
			break
		}
		if nextErr != nil {
			return nil, perrf("invalid multipart body: %v", nextErr)
		}
		if part.FileName() == "" && (part.FormName() == "model" || part.FormName() == "prompt") {
			value, readErr := io.ReadAll(io.LimitReader(part, 1<<20))
			if readErr != nil {
				part.Close() // #nosec G104 -- error-path cleanup; the original error is returned
				return nil, perrf("read multipart field: %v", readErr)
			}
			if part.FormName() == "model" {
				model = string(value)
			} else {
				prompt = string(value)
			}
		}
		part.Close() // #nosec G104 -- cleanup of a fully consumed multipart part
	}
	if strings.TrimSpace(model) == "" {
		return nil, perrf("model is required")
	}
	rc := &domain.RequestContract{
		Protocol: domain.ProtocolOpenAIImages, Origin: domain.ProtocolOpenAIImages,
		Model: strings.TrimSpace(model), Native: json.RawMessage(body),
		NativeContentType: contentType, Operation: "edit",
	}
	if strings.TrimSpace(prompt) != "" {
		rc.Messages = []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{{Kind: domain.BlockText, Text: prompt}}}}
	}
	return rc, nil
}

func (h *OpenAIImagesHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	w, r, finishObservation := observeIngress(h.Eng, domain.ProtocolOpenAIImages, w, r)
	defer finishObservation()

	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method_not_allowed", "POST only")
		return
	}
	body, err := readAll(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "read body: "+err.Error())
		return
	}
	if len(body) == 0 {
		writeError(w, http.StatusBadRequest, "invalid_request_error", "empty request body")
		return
	}
	observeIngressPhase(r.Context(), "BODY_PARSE_STARTED", nil)
	var rc *domain.RequestContract
	if strings.HasSuffix(r.URL.Path, "/edits") {
		rc, err = h.toEditContract(body, r.Header.Get("Content-Type"))
	} else {
		rc, err = h.toGenerationContract(body)
	}
	finishIngressPhase(r.Context(), "BODY_PARSE", err)
	if err != nil {
		var parse *parseError
		status := http.StatusBadRequest
		if !errors.As(err, &parse) {
			status = http.StatusInternalServerError
		}
		writeError(w, status, "invalid_request_error", err.Error())
		return
	}
	result, failure := h.Eng.Execute(r.Context(), rc, requestMeta(r, rc))
	if failure != nil {
		writeFailure(w, failure)
		return
	}
	if result == nil || result.Response == nil || len(result.Response.NativeBody) == 0 {
		writeError(w, http.StatusBadGateway, "server_error", "upstream returned an empty image response")
		return
	}
	contentType := result.Response.NativeContentType
	if contentType == "" {
		contentType = "application/json"
	}
	if !writeNativeJSONResponse(w, result.Response.NativeBody, contentType) {
		writeError(w, http.StatusBadGateway, "server_error", "upstream returned an invalid image response")
	}
}
