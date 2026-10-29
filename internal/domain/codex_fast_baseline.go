package domain

// Pinned from the official Codex catalog, not an OpenCodeX-generated cache.
// The digest covers the original models.json. Updating this baseline requires
// reviewing both service_tiers and minimal_client_version at the pinned commit.
const (
	CodexFastBaselineRevision = "cf6754e6828db447c288d72ca188d3dbc46feb56"
	CodexFastBaselineSource   = "https://github.com/openai/codex/blob/" + CodexFastBaselineRevision + "/codex-rs/models-manager/models.json"
	CodexFastBaselineSHA256   = "cfab95ffae957d49aa3ebe4520db56774a7339c77da67668b0222ce77e93d6f5"
)

// CodexModelFastBaseline describes model-level support only. It must never be
// copied into connection capabilities: it is not account entitlement evidence.
type CodexModelFastBaseline struct {
	Priority             bool
	MinimalClientVersion string
}

var codexFastBaseline = map[string]CodexModelFastBaseline{
	"gpt-6-astra":              {Priority: true, MinimalClientVersion: "0.153.0"},
	"gpt-5.6-sol":              {Priority: true, MinimalClientVersion: "0.144.0"},
	"gpt-5.6-terra":            {Priority: true, MinimalClientVersion: "0.144.0"},
	"gpt-5.6-luna":             {Priority: true, MinimalClientVersion: "0.144.0"},
	"gpt-daybreak-blue-latest": {MinimalClientVersion: "0.142.2"},
	"gpt-daybreak-red-latest":  {MinimalClientVersion: "0.142.2"},
	"gpt-5.5":                  {Priority: true, MinimalClientVersion: "0.124.0"},
	"gpt-5.4":                  {Priority: true, MinimalClientVersion: "0.98.0"},
	"codex-auto-review":        {Priority: true, MinimalClientVersion: "0.98.0"},
}

// CodexFastBaseline returns the pinned model-level declaration. A missing row
// is unknown, not unsupported. Neither discovery nor routing uses this table
// to admit a model or enable Priority.
func CodexFastBaseline(model string) (CodexModelFastBaseline, bool) {
	entry, ok := codexFastBaseline[model]
	return entry, ok
}
