package domain

import "strings"

const (
	// CodexClientVersion identifies the compatible Codex wire contract used by
	// both inference requests and the account-scoped remote model catalog.
	// Keep this at least as new as the pinned catalog's minimal_client_version.
	CodexClientVersion = "0.153.0"
	CodexOriginator    = "codex-tui"
	CodexUserAgent     = "codex-tui/" + CodexClientVersion + " (Mac OS 26.5.0; arm64) iTerm.app/3.6.10 (codex-tui; " + CodexClientVersion + ")"
)

// Built-in ChatGPT/Codex subscription catalog (Phase 4). A cliproxy-codex
// channel that declares no models expands to the list for its account's plan
// tier (id_token chatgpt_plan_type); the data mirrors the Codex CLI's
// per-plan availability tables. Operators can always declare models
// explicitly instead — explicit lists replace the catalog, never mix.

type codexModelSpec struct {
	ContextWindow int64
	MaxOutput     int64
	Vision        bool
	Images        bool
}

// codexCatalog is the fixed per-model capability data. Text models stream,
// call tools (incl. parallel), reason, and support structured output; GPT
// Image is image-only and deliberately non-streaming.
var codexCatalog = map[string]codexModelSpec{
	"gpt-5.4":             {ContextWindow: 272000, MaxOutput: 128000, Vision: true},
	"gpt-5.4-mini":        {ContextWindow: 272000, MaxOutput: 128000, Vision: true},
	"gpt-5.5":             {ContextWindow: 272000, MaxOutput: 128000, Vision: true},
	"gpt-5.6-sol":         {ContextWindow: 372000, MaxOutput: 128000, Vision: true},
	"gpt-5.6-terra":       {ContextWindow: 372000, MaxOutput: 128000, Vision: true},
	"gpt-5.6-luna":        {ContextWindow: 372000, MaxOutput: 128000, Vision: true},
	"gpt-6-astra":         {ContextWindow: 372000, MaxOutput: 128000, Vision: true},
	"gpt-5.3-codex-spark": {ContextWindow: 128000, MaxOutput: 128000, Vision: false},
	"codex-auto-review":   {ContextWindow: 272000, MaxOutput: 128000, Vision: true},
	"gpt-image-2":         {Images: true},
}

// CodexModelCapabilities returns the authoritative capability contract for a
// built-in Codex model. Management and compilation use this instead of
// trusting a stale console payload; unknown models remain operator-defined so
// newly released models can be adopted before this catalog is updated.
func CodexModelCapabilities(model string) (Capabilities, bool) {
	spec, ok := codexCatalog[model]
	if !ok {
		return Capabilities{}, false
	}
	if spec.Images {
		return Capabilities{Images: true}, true
	}
	return Capabilities{
		Text: true, Stream: true, Tools: true, ParallelTools: true,
		Reasoning: true, StructuredOutput: true, Vision: spec.Vision,
		ContextWindow: spec.ContextWindow, MaxOutput: spec.MaxOutput,
	}, true
}

// codexPlanModels lists the model names available per plan tier; slice order
// becomes the client-facing listing order.
var codexPlanModels = map[string][]string{
	"free": {"gpt-5.4-mini", "gpt-5.5", "gpt-5.6-terra", "gpt-5.6-luna", "codex-auto-review"},
	"team": {"gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra",
		"gpt-5.6-luna", "gpt-6-astra", "codex-auto-review", "gpt-image-2"},
	"plus": {"gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra",
		"gpt-5.6-luna", "gpt-6-astra", "codex-auto-review", "gpt-5.3-codex-spark", "gpt-image-2"},
	"pro": {"gpt-5.4", "gpt-5.4-mini", "gpt-5.5", "gpt-5.6-sol", "gpt-5.6-terra",
		"gpt-5.6-luna", "gpt-6-astra", "codex-auto-review", "gpt-5.3-codex-spark", "gpt-image-2"},
}

// CodexModelNativeProtocol hides the Codex backend's per-model wire choice
// from configuration clients. Known image models use the Images API; Codex
// text models continue to use Responses.
func CodexModelNativeProtocol(model string) Protocol {
	capabilities, known := CodexModelCapabilities(strings.TrimSpace(model))
	if known && capabilities.Images && !capabilities.Text {
		return ProtocolOpenAIImages
	}
	return ProtocolOpenAIResponses
}

// codexSyncedMaxOutput is the output ceiling Ferry assumes for a Codex
// text model whose remote catalog entry discloses no limit. It mirrors the
// built-in catalog's uniform ceiling.
const codexSyncedMaxOutput int64 = 128000

// CodexCatalogModelCapabilities derives a usable capability contract for a
// Codex model the built-in catalog does not yet know, from the account-scoped
// remote catalog metadata. Image-only input maps to the Images API contract;
// every other model keeps the standard Codex text contract (stream, tools
// incl. parallel, reasoning, structured output). An undisclosed context
// window stays 0, which capability matching treats as no advertised minimum.
// The built-in catalog stays authoritative for known models; this contract
// only covers models adopted through provider model sync before the embedded
// catalog catches up.
func CodexCatalogModelCapabilities(imageOnly, imageInput bool, contextWindow int64) Capabilities {
	if imageOnly {
		return Capabilities{Images: true}
	}
	return Capabilities{
		Text: true, Stream: true, Tools: true, ParallelTools: true,
		Reasoning: true, StructuredOutput: true, Vision: imageInput,
		ContextWindow: contextWindow, MaxOutput: codexSyncedMaxOutput,
	}
}

// CodexCatalogModelProtocol resolves the native wire protocol for an unknown
// Codex model from remote catalog metadata: image-only models speak the
// Images API; everything else keeps the Responses default.
func CodexCatalogModelProtocol(imageOnly bool) Protocol {
	if imageOnly {
		return ProtocolOpenAIImages
	}
	return ProtocolOpenAIResponses
}

// CodexModelsForPlan synthesizes the channel models offered by one plan tier.
// Unknown or unset tiers degrade to the free list (least privilege — a
// missing model can be added back via an explicit channel model, but a
// phantom model would fail at request time).
func CodexModelsForPlan(tier string) []ChannelModel {
	tier = codexPlanCatalog(tier)
	names, ok := codexPlanModels[tier]
	if !ok {
		names = codexPlanModels["free"]
	}
	out := make([]ChannelModel, 0, len(names))
	for _, n := range names {
		capabilities, _ := CodexModelCapabilities(n)
		out = append(out, ChannelModel{
			Model:        n,
			Protocols:    []Protocol{CodexModelNativeProtocol(n)},
			Capabilities: capabilities,
		})
	}
	return out
}

// CodexModelSupportedForPlan reports entitlement for built-in catalog models.
// Unknown explicit models remain allowed so an operator can adopt a newly
// released Codex model before the embedded catalog catches up. Unknown or
// missing plan claims use the least-privilege free catalog.
func CodexModelSupportedForPlan(tier, model string) bool {
	if _, known := codexCatalog[model]; !known {
		return true
	}
	tier = codexPlanCatalog(tier)
	names, ok := codexPlanModels[tier]
	if !ok {
		names = codexPlanModels["free"]
	}
	for _, candidate := range names {
		if candidate == model {
			return true
		}
	}
	return false
}

// codexPlanCatalog mirrors the subscription aliases used by the Codex OAuth
// service. Business and Go accounts use the Team supply table; an empty or
// unrecognized claim remains least-privilege until explicitly configured.
func codexPlanCatalog(tier string) string {
	switch strings.ToLower(strings.TrimSpace(tier)) {
	case "business", "go", "self_serve_business_usage_based":
		return "team"
	case "free_workspace":
		return "free"
	case "free", "team", "plus", "pro":
		return strings.ToLower(strings.TrimSpace(tier))
	default:
		return "free"
	}
}

// CodexModelNamesForPlan exposes a copy of the plan catalog for management
// discovery without leaking the mutable package slices.
func CodexModelNamesForPlan(tier string) []string {
	models := CodexModelsForPlan(tier)
	out := make([]string, 0, len(models))
	for _, model := range models {
		out = append(out, model.Model)
	}
	return out
}

// CodexModelNames returns the complete built-in discovery catalog. It is used
// before OAuth finishes, when the account plan is not known yet. Once an
// authenticated account is available, management discovery narrows the list
// to CodexModelNamesForPlan instead of guessing an entitlement tier.
func CodexModelNames() []string {
	// The pro table currently contains every known built-in model in the
	// intended client-facing order. Still merge the other tables so a future
	// plan-only addition cannot disappear from pre-auth discovery.
	tiers := []string{"pro", "plus", "team", "free"}
	seen := map[string]bool{}
	out := []string{}
	for _, tier := range tiers {
		for _, model := range codexPlanModels[tier] {
			if !seen[model] {
				seen[model] = true
				out = append(out, model)
			}
		}
	}
	return out
}
