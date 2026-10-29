package management

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ferry/internal/domain"
)

func TestVolcengineCatalogRequestMatchesOfficialSign4OracleFixture(t *testing.T) {
	// These fixed values were independently derived with Volcengine's official
	// Go Sign4 implementation. The Plan API publishes no reusable credentialed
	// signature vector, so this is an official-implementation oracle fixture.
	now := time.Date(2026, 6, 2, 8, 55, 37, 0, time.UTC)
	identity := volcengineSigningIdentity{accessKeyID: "testAK", signingKey: "testSK"}
	req, err := newVolcengineCatalogRequest(context.Background(), domain.ProviderModelSyncCatalogCodingPlan, identity, now)
	if err != nil {
		t.Fatal(err)
	}
	body, err := io.ReadAll(req.Body)
	if err != nil {
		t.Fatal(err)
	}
	if req.Method != http.MethodPost || req.URL.String() != "https://ark.cn-beijing.volces.com/?Action=ListArkCodingPlanModel&Version=2024-01-01" ||
		string(body) != `{}` || req.Header.Get("Content-Type") != "application/json; charset=UTF-8" ||
		req.Header.Get("X-Date") != "20260602T085537Z" ||
		req.Header.Get("X-Content-Sha256") != "44136fa355b3678a1146ad16f7e8649e94fb4fc21fe77e8310c060f61caaff8a" {
		t.Fatalf("request=%s %s headers=%v body=%q", req.Method, req.URL, req.Header, body)
	}
	wantAuthorization := "HMAC-SHA256 Credential=testAK/20260602/cn-beijing/ark_stg/request, SignedHeaders=content-type;host;x-content-sha256;x-date, Signature=4daf3953227ffd9587c871268b80411e5e0b0f92eac02134260d49a71d400faf"
	if req.Header.Get("Authorization") != wantAuthorization {
		t.Fatalf("authorization=%q want=%q", req.Header.Get("Authorization"), wantAuthorization)
	}

	identity.sessionToken = "testSessionToken"
	req, err = newVolcengineCatalogRequest(context.Background(), domain.ProviderModelSyncCatalogCodingPlan, identity, now)
	if err != nil {
		t.Fatal(err)
	}
	wantAuthorization = "HMAC-SHA256 Credential=testAK/20260602/cn-beijing/ark_stg/request, SignedHeaders=content-type;host;x-content-sha256;x-date;x-security-token, Signature=ed16c51327eaddb1f01b58b70b908b2a2539a19cb4343ef3f3d82e9c4d47e563"
	if req.Header.Get("X-Security-Token") != identity.sessionToken || req.Header.Get("Authorization") != wantAuthorization {
		t.Fatalf("STS headers=%v", req.Header)
	}
}

func TestDiscoverVolcenginePlanModelsUsesFixedControlPlaneAndOfficialSchema(t *testing.T) {
	var calls int
	client := &http.Client{Transport: modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		calls++
		if request.URL.Host != "ark.cn-beijing.volces.com" || request.URL.Path != "/" ||
			request.URL.Query().Get("Action") != "ListArkAgentPlanModel" ||
			request.URL.Query().Get("Version") != volcengineCatalogVersion ||
			!strings.HasPrefix(request.Header.Get("Authorization"), "HMAC-SHA256 Credential=testAK/") {
			t.Fatalf("request=%s headers=%v", request.URL, request.Header)
		}
		return modelCatalogResponse(http.StatusOK, `{
			"ResponseMetadata":{"Service":"ark_stg"},
			"Result":{"Datas":[{"ModelID":"doubao-seed-1.6"},{"ModelID":" doubao-seed-1.6 "},{"ModelID":"doubao-new"}]}
		}`), nil
	})}
	models, err := discoverVolcenginePlanModels(context.Background(), client, domain.ProviderModelSyncCatalogAgentPlan,
		volcengineSigningIdentity{accessKeyID: "testAK", signingKey: "testSK"}, time.Date(2026, 6, 2, 8, 55, 37, 0, time.UTC))
	if err != nil || calls != 1 || len(models) != 2 || models[0] != "doubao-new" || models[1] != "doubao-seed-1.6" {
		t.Fatalf("models=%v calls=%d err=%v", models, calls, err)
	}
}

func TestDiscoverVolcenginePlanModelsRejectsApplicationErrorWithData(t *testing.T) {
	client := &http.Client{Transport: modelCatalogRoundTripFunc(func(*http.Request) (*http.Response, error) {
		return modelCatalogResponse(http.StatusOK, `{
			"ResponseMetadata":{"Error":{"Code":"AccessDenied","Message":"must not escape"}},
			"Result":{"Datas":[{"ModelID":"must-not-publish"}]}
		}`), nil
	})}
	models, err := discoverVolcenginePlanModels(context.Background(), client, domain.ProviderModelSyncCatalogCodingPlan,
		volcengineSigningIdentity{accessKeyID: "testAK", signingKey: "testSK"}, time.Date(2026, 6, 2, 8, 55, 37, 0, time.UTC))
	if err == nil || len(models) != 0 || strings.Contains(err.Error(), "AccessDenied") || strings.Contains(err.Error(), "must not escape") {
		t.Fatalf("models=%v err=%v", models, err)
	}
}

func TestParseVolcengineSigningIdentityRejectsUnsafeOrIncompleteJSON(t *testing.T) {
	valid, err := parseVolcengineSigningIdentity(`{"access_key_id":"ak","secret_access_key":"sk","session_token":"token"}`)
	if err != nil || valid.accessKeyID != "ak" || valid.signingKey != "sk" || valid.sessionToken != "token" {
		t.Fatalf("valid identity=%+v err=%v", valid, err)
	}
	for _, raw := range []string{
		`{}`,
		`{"access_key_id":"ak"}`,
		`{"access_key_id":"ak","secret_access_key":"sk","extra":"value"}`,
		`{"access_key_id":"ak","secret_access_key":"sk","session_token":null}`,
		`{"access_key_id":"ak\n","secret_access_key":"sk"}`,
		`{"access_key_id":"ak","secret_access_key":7}`,
	} {
		if _, err := parseVolcengineSigningIdentity(raw); err == nil {
			t.Fatalf("credential %s was accepted", raw)
		}
	}
}
