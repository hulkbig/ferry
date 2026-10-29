package management

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"
	"unicode"

	"ferry/internal/domain"
)

const (
	volcengineCatalogEndpoint = "https://ark.cn-beijing.volces.com/"
	volcengineCatalogRegion   = "cn-beijing"
	volcengineCatalogService  = "ark_stg"
	volcengineCatalogVersion  = "2024-01-01"
)

var volcengineCatalogBody = []byte("{}")

type volcengineSigningIdentity struct {
	accessKeyID  string
	signingKey   string
	sessionToken string
}

func parseVolcengineSigningIdentity(raw string) (volcengineSigningIdentity, error) {
	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &fields); err != nil {
		return volcengineSigningIdentity{}, fmt.Errorf("catalog credential is not valid JSON")
	}
	identity := volcengineSigningIdentity{}
	for name, value := range fields {
		if strings.TrimSpace(string(value)) == "null" {
			return volcengineSigningIdentity{}, fmt.Errorf("catalog credential fields must be strings")
		}
		var decoded string
		if err := json.Unmarshal(value, &decoded); err != nil {
			return volcengineSigningIdentity{}, fmt.Errorf("catalog credential fields must be strings")
		}
		switch name {
		case "access_key_id":
			identity.accessKeyID = decoded
		case "secret_access_key":
			identity.signingKey = decoded
		case "session_token":
			identity.sessionToken = decoded
		default:
			return volcengineSigningIdentity{}, fmt.Errorf("catalog credential contains an unknown field")
		}
	}
	if !validVolcengineCredentialPart(identity.accessKeyID, true) ||
		!validVolcengineCredentialPart(identity.signingKey, true) ||
		!validVolcengineCredentialPart(identity.sessionToken, false) {
		return volcengineSigningIdentity{}, fmt.Errorf("catalog credential is incomplete or invalid")
	}
	return identity, nil
}

func validVolcengineCredentialPart(value string, required bool) bool {
	if value == "" {
		return !required
	}
	if strings.TrimSpace(value) != value {
		return false
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

func volcengineCatalogAction(plan domain.ProviderModelSyncCatalogPlan) (string, error) {
	switch plan {
	case domain.ProviderModelSyncCatalogAgentPlan:
		return "ListArkAgentPlanModel", nil
	case domain.ProviderModelSyncCatalogCodingPlan:
		return "ListArkCodingPlanModel", nil
	default:
		return "", fmt.Errorf("unsupported Volcengine plan catalog")
	}
}

func newVolcengineCatalogRequest(ctx context.Context, plan domain.ProviderModelSyncCatalogPlan, identity volcengineSigningIdentity, now time.Time) (*http.Request, error) {
	action, err := volcengineCatalogAction(plan)
	if err != nil {
		return nil, err
	}
	requestURL, _ := url.Parse(volcengineCatalogEndpoint)
	query := requestURL.Query()
	query.Set("Action", action)
	query.Set("Version", volcengineCatalogVersion)
	requestURL.RawQuery = query.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, requestURL.String(), bytes.NewReader(volcengineCatalogBody))
	if err != nil {
		return nil, fmt.Errorf("build Volcengine catalog request")
	}
	req.Header.Set("Content-Type", "application/json; charset=UTF-8")
	signVolcengineCatalogRequest(req, identity, now.UTC())
	return req, nil
}

func signVolcengineCatalogRequest(req *http.Request, identity volcengineSigningIdentity, now time.Time) {
	timestamp := now.UTC().Format("20060102T150405Z")
	date := timestamp[:8]
	bodyDigest := sha256.Sum256(volcengineCatalogBody)
	payloadHash := hex.EncodeToString(bodyDigest[:])
	req.Header.Set("X-Date", timestamp)
	req.Header.Set("X-Content-Sha256", payloadHash)
	if identity.sessionToken != "" {
		req.Header.Set("X-Security-Token", identity.sessionToken)
	}
	headerValues := map[string]string{
		"content-type":     req.Header.Get("Content-Type"),
		"host":             req.URL.Host,
		"x-content-sha256": payloadHash,
		"x-date":           timestamp,
	}
	if identity.sessionToken != "" {
		headerValues["x-security-token"] = identity.sessionToken
	}
	headerNames := make([]string, 0, len(headerValues))
	for name := range headerValues {
		headerNames = append(headerNames, name)
	}
	sort.Strings(headerNames)
	var canonicalHeaders strings.Builder
	for _, name := range headerNames {
		canonicalHeaders.WriteString(name)
		canonicalHeaders.WriteByte(':')
		canonicalHeaders.WriteString(strings.TrimSpace(headerValues[name]))
		canonicalHeaders.WriteByte('\n')
	}
	signedHeaders := strings.Join(headerNames, ";")
	canonicalRequest := strings.Join([]string{
		req.Method,
		"/",
		strings.ReplaceAll(req.URL.Query().Encode(), "+", "%20"),
		canonicalHeaders.String(),
		signedHeaders,
		payloadHash,
	}, "\n")
	canonicalDigest := sha256.Sum256([]byte(canonicalRequest))
	scope := strings.Join([]string{date, volcengineCatalogRegion, volcengineCatalogService, "request"}, "/")
	stringToSign := strings.Join([]string{"HMAC-SHA256", timestamp, scope, hex.EncodeToString(canonicalDigest[:])}, "\n")
	dateKey := hmacSHA256([]byte(identity.signingKey), date)
	regionKey := hmacSHA256(dateKey, volcengineCatalogRegion)
	serviceKey := hmacSHA256(regionKey, volcengineCatalogService)
	requestKey := hmacSHA256(serviceKey, "request")
	signature := hex.EncodeToString(hmacSHA256(requestKey, stringToSign))
	req.Header.Set("Authorization", "HMAC-SHA256 Credential="+identity.accessKeyID+"/"+scope+
		", SignedHeaders="+signedHeaders+", Signature="+signature)
}

func hmacSHA256(key []byte, value string) []byte {
	mac := hmac.New(sha256.New, key)
	_, _ = mac.Write([]byte(value))
	return mac.Sum(nil)
}

func discoverVolcenginePlanModels(ctx context.Context, client *http.Client, plan domain.ProviderModelSyncCatalogPlan, identity volcengineSigningIdentity, now time.Time) ([]string, error) {
	req, err := newVolcengineCatalogRequest(ctx, plan, identity, now)
	if err != nil {
		return nil, err
	}
	safeClient := *client
	safeClient.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	resp, err := safeClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("Volcengine model catalog request failed")
	}
	body, readErr := io.ReadAll(io.LimitReader(resp.Body, 2<<20+1))
	_ = resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return nil, &providerModelDiscoveryHTTPError{Status: resp.StatusCode}
	}
	if readErr != nil {
		return nil, fmt.Errorf("read Volcengine model catalog")
	}
	if len(body) > 2<<20 {
		return nil, fmt.Errorf("Volcengine model catalog response exceeds 2 MiB")
	}
	var envelope struct {
		ResponseMetadata struct {
			Error json.RawMessage `json:"Error"`
		} `json:"ResponseMetadata"`
		Result struct {
			Datas []struct {
				ModelID string `json:"ModelID"`
			} `json:"Datas"`
		} `json:"Result"`
	}
	if err := json.Unmarshal(body, &envelope); err != nil {
		return nil, fmt.Errorf("Volcengine model catalog is not valid JSON")
	}
	if rawError := strings.TrimSpace(string(envelope.ResponseMetadata.Error)); rawError != "" && rawError != "null" {
		return nil, fmt.Errorf("Volcengine model catalog returned an application error")
	}
	models := make([]string, 0, len(envelope.Result.Datas))
	seen := map[string]bool{}
	for _, item := range envelope.Result.Datas {
		modelID := strings.TrimSpace(item.ModelID)
		if modelID == "" || seen[modelID] || !validSynchronizedModelID(modelID) {
			continue
		}
		if len(models) == maxDiscoveredModels {
			return nil, fmt.Errorf("Volcengine model catalog exceeds %d models", maxDiscoveredModels)
		}
		seen[modelID] = true
		models = append(models, modelID)
	}
	if len(models) == 0 {
		return nil, fmt.Errorf("Volcengine model catalog returned no models")
	}
	sort.Strings(models)
	return models, nil
}
