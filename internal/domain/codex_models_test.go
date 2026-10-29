package domain

import "testing"

func TestCodexModelsForPlan(t *testing.T) {
	pro := CodexModelsForPlan("pro")
	if len(pro) != 10 {
		t.Fatalf("pro model count = %d, want 10 (incl. spark and GPT Image)", len(pro))
	}
	spark, image := -1, -1
	for i, m := range pro {
		switch m.Model {
		case "gpt-image-2":
			image = i
			if len(m.Protocols) != 1 || m.Protocols[0] != ProtocolOpenAIImages {
				t.Errorf("%s protocols = %v", m.Model, m.Protocols)
			}
			if !m.Capabilities.Images || m.Capabilities.Text || m.Capabilities.Stream ||
				m.Capabilities.Tools || m.Capabilities.ParallelTools || m.Capabilities.Reasoning ||
				m.Capabilities.StructuredOutput || m.Capabilities.Vision {
				t.Errorf("%s capabilities = %+v", m.Model, m.Capabilities)
			}
			continue
		case "gpt-5.3-codex-spark":
			spark = i
			m := pro[i]
			if m.Capabilities.Vision {
				t.Error("spark must be text-only")
			}
			if m.Capabilities.ContextWindow != 128000 || m.Capabilities.MaxOutput != 128000 {
				t.Errorf("spark windows = %d/%d", m.Capabilities.ContextWindow, m.Capabilities.MaxOutput)
			}
		default:
			if !m.Capabilities.Vision {
				t.Errorf("%s: vision unexpectedly false", m.Model)
			}
		}
		if m.Protocols[0] != ProtocolOpenAIResponses || len(m.Protocols) != 1 {
			t.Errorf("%s protocols = %v", m.Model, m.Protocols)
		}
		if !m.Capabilities.Stream || !m.Capabilities.Tools ||
			!m.Capabilities.ParallelTools || !m.Capabilities.Reasoning ||
			!m.Capabilities.StructuredOutput {
			t.Errorf("%s missing base capabilities: %+v", m.Model, m.Capabilities)
		}
	}
	if spark < 0 {
		t.Fatal("spark missing from pro list")
	}
	if image < 0 {
		t.Fatal("gpt-image-2 missing from pro list")
	}
	free := CodexModelsForPlan("free")
	for _, m := range free {
		if m.Model == "gpt-5.4" || m.Model == "gpt-5.6-sol" || m.Model == "gpt-6-astra" || m.Model == "gpt-5.3-codex-spark" || m.Model == "gpt-image-2" {
			t.Errorf("free tier offers paid model %s", m.Model)
		}
	}
	if got := CodexModelsForPlan("mystery-tier"); len(got) != len(free) {
		t.Errorf("unknown tier should degrade to free list, got %d models", len(got))
	}
	if got := CodexModelsForPlan(""); len(got) != len(free) {
		t.Errorf("empty tier should degrade to free list, got %d models", len(got))
	}
	if got, team := CodexModelsForPlan("business"), CodexModelsForPlan("team"); len(got) != len(team) {
		t.Errorf("business alias should use team catalog, got %d want %d", len(got), len(team))
	}
	if got, team := CodexModelsForPlan("go"), CodexModelsForPlan("team"); len(got) != len(team) {
		t.Errorf("go alias should use team catalog, got %d want %d", len(got), len(team))
	}
}

func TestCodexModelSupportedForPlan(t *testing.T) {
	if CodexModelSupportedForPlan("free", "gpt-5.6-sol") {
		t.Fatal("free plan accepted a pro/team model")
	}
	if !CodexModelSupportedForPlan("pro", "gpt-5.6-sol") {
		t.Fatal("pro plan rejected gpt-5.6-sol")
	}
	for _, plan := range []string{"team", "plus", "pro", "business", "go"} {
		if !CodexModelSupportedForPlan(plan, "gpt-6-astra") {
			t.Errorf("%s plan rejected gpt-6-astra", plan)
		}
	}
	if CodexModelSupportedForPlan("free", "gpt-6-astra") {
		t.Fatal("free plan accepted gpt-6-astra")
	}
	if CodexModelSupportedForPlan("free", "gpt-image-2") {
		t.Fatal("free plan accepted gpt-image-2")
	}
	for _, plan := range []string{"team", "plus", "pro", "business", "go"} {
		if !CodexModelSupportedForPlan(plan, "gpt-image-2") {
			t.Errorf("%s plan rejected gpt-image-2", plan)
		}
	}
	if !CodexModelSupportedForPlan("free", "future-codex-model") {
		t.Fatal("explicit unknown model must remain usable ahead of catalog refresh")
	}
}

func TestCodexPreAuthCatalogIncludesSolAndGPTImage(t *testing.T) {
	models := CodexModelNames()
	foundSol, foundImage := false, false
	for _, model := range models {
		foundSol = foundSol || model == "gpt-5.6-sol"
		foundImage = foundImage || model == "gpt-image-2"
	}
	if !foundSol || !foundImage {
		t.Fatalf("pre-auth catalog omitted required models: %v", models)
	}
}

func TestCodexModelCapabilitiesAreAuthoritative(t *testing.T) {
	capabilities, ok := CodexModelCapabilities("gpt-5.6-sol")
	if !ok || !capabilities.Text || !capabilities.Stream || !capabilities.Tools ||
		!capabilities.ParallelTools || !capabilities.Reasoning || !capabilities.StructuredOutput ||
		!capabilities.Vision ||
		capabilities.ContextWindow != 372000 || capabilities.MaxOutput != 128000 {
		t.Fatalf("gpt-5.6-sol capabilities=%+v known=%v", capabilities, ok)
	}
	if _, ok := CodexModelCapabilities("future-codex-model"); ok {
		t.Fatal("unknown future model must remain operator-defined")
	}
	astra, ok := CodexModelCapabilities("gpt-6-astra")
	if !ok || astra != capabilities {
		t.Fatalf("gpt-6-astra capabilities=%+v known=%v, want identical to gpt-5.6-sol", astra, ok)
	}
	if got := CodexModelNativeProtocol("gpt-6-astra"); got != ProtocolOpenAIResponses {
		t.Fatalf("gpt-6-astra native protocol=%q", got)
	}
	image, ok := CodexModelCapabilities("gpt-image-2")
	if !ok || !image.Images || image.Text || image.Stream || image.Tools ||
		image.StructuredOutput || image.Vision {
		t.Fatalf("gpt-image-2 capabilities=%+v known=%v", image, ok)
	}
	if got := CodexModelNativeProtocol("gpt-image-2"); got != ProtocolOpenAIImages {
		t.Fatalf("gpt-image-2 native protocol=%q", got)
	}
	if got := CodexModelNativeProtocol("gpt-5.6-sol"); got != ProtocolOpenAIResponses {
		t.Fatalf("gpt-5.6-sol native protocol=%q", got)
	}
}

func TestCodexCatalogModelDerivations(t *testing.T) {
	text := CodexCatalogModelCapabilities(false, true, 372000)
	if !text.Text || !text.Stream || !text.Tools || !text.ParallelTools || !text.Reasoning ||
		!text.StructuredOutput || !text.Vision || text.Images ||
		text.ContextWindow != 372000 || text.MaxOutput != 128000 {
		t.Fatalf("text derivation = %+v", text)
	}
	undisclosed := CodexCatalogModelCapabilities(false, false, 0)
	if undisclosed.Vision || undisclosed.ContextWindow != 0 || undisclosed.MaxOutput != 128000 {
		t.Fatalf("undisclosed derivation = %+v", undisclosed)
	}
	image := CodexCatalogModelCapabilities(true, true, 0)
	if !image.Images || image.Text || image.Stream || image.Tools || image.Vision {
		t.Fatalf("image-only derivation = %+v", image)
	}
	if got := CodexCatalogModelProtocol(true); got != ProtocolOpenAIImages {
		t.Fatalf("image-only protocol = %q", got)
	}
	if got := CodexCatalogModelProtocol(false); got != ProtocolOpenAIResponses {
		t.Fatalf("text protocol = %q", got)
	}
}
