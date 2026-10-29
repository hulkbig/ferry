package policy

import (
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
)

type capabilityEvidenceTestKey struct {
	providerID          string
	endpointFingerprint string
	model               string
	protocol            domain.Protocol
	capability          string
}

func stableHashV2Config(secret string) *config.File {
	return &config.File{
		Server:         config.ServerConfig{Timezone: "Asia/Shanghai"},
		V2DomainSource: config.V2DomainSourceNative,
		LogicalModels: []domain.LogicalModel{{
			ID: "smart", DisplayName: "Smart", Enabled: true,
			DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{
				ProviderID: "provider", UpstreamModel: "model-a",
				RecoverOn: []domain.FailureType{domain.FailProvider5xx, domain.FailConnect},
			}}},
			Overrides: []domain.OverrideSpec{{
				ID: "caller-route", Name: "Caller Route", Enabled: true,
				Callers: []string{"caller-b", "caller-a"}, Mode: domain.OverrideCustomRoute,
				Route: &domain.RouteSpec{Steps: []domain.RouteStep{{
					ProviderID: "provider", UpstreamModel: "model-b",
				}}},
			}},
		}},
		Providers: []domain.Provider{{
			ID: "provider", Name: "Provider", Type: domain.ProviderCustom,
			BaseURL: "https://example.test/v1", Enabled: true,
			Connections: []domain.ConnectionRef{{ID: "account"}},
		}},
		Connections: []domain.Connection{{
			ID: "account", ProviderID: "provider", Identity: "Account",
			Credential: domain.CredentialRef{SecretRef: "plain:" + secret}, Enabled: true,
			Models: []domain.ConnectionModel{
				{UpstreamModel: "model-b", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses}, Capabilities: domain.Capabilities{Text: true}},
				{UpstreamModel: "model-a", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}, Capabilities: domain.Capabilities{Text: true, Tools: true}},
			},
		}},
	}
}

func TestCompileAtRevisionHashIsStableAcrossRepublish(t *testing.T) {
	compiler := NewCompiler()
	const revision = 23
	first, err := compiler.CompileAtRevision(stableHashV2Config("first-secret"), revision)
	if err != nil {
		t.Fatal(err)
	}
	second, err := compiler.CompileAtRevision(stableHashV2Config("first-secret"), revision)
	if err != nil {
		t.Fatal(err)
	}
	if first.Hash != second.Hash {
		t.Fatalf("same revision/config republish changed hash: %q != %q", first.Hash, second.Hash)
	}
	credentialRefresh, err := compiler.CompileAtRevision(stableHashV2Config("rotated-secret"), revision)
	if err != nil {
		t.Fatal(err)
	}
	if credentialRefresh.Hash != first.Hash {
		t.Fatalf("credential material changed configuration hash: %q != %q", credentialRefresh.Hash, first.Hash)
	}

	routeChanged := stableHashV2Config("first-secret")
	routeChanged.LogicalModels[0].DefaultRoute.Steps[0].UpstreamModel = "model-b"
	routeSnapshot, err := compiler.CompileAtRevision(routeChanged, revision)
	if err != nil {
		t.Fatal(err)
	}
	if routeSnapshot.Hash == first.Hash {
		t.Fatal("V2 default route change did not change configuration hash")
	}

	overrideChanged := stableHashV2Config("first-secret")
	overrideChanged.LogicalModels[0].Overrides[0].Route.Steps[0].UpstreamModel = "model-a"
	overrideSnapshot, err := compiler.CompileAtRevision(overrideChanged, revision)
	if err != nil {
		t.Fatal(err)
	}
	if overrideSnapshot.Hash == first.Hash {
		t.Fatal("V2 override change did not change configuration hash")
	}

	exactOnlyChanged := stableHashV2Config("first-secret")
	exactOnlyChanged.LogicalModels[0].ExactOnly = true
	exactOnlySnapshot, err := compiler.CompileAtRevision(exactOnlyChanged, revision)
	if err != nil {
		t.Fatal(err)
	}
	if exactOnlySnapshot.Hash == first.Hash {
		t.Fatal("V2 exact-only policy change did not change configuration hash")
	}

	migrationChanged := stableHashV2Config("first-secret")
	migrationChanged.V2MigrationIssues = []domain.LegacyMappingIssue{{
		LogicalModelID: "smart", Kind: "legacy_rule", ObjectID: "conditional", Blocking: true,
	}}
	migrationSnapshot, err := compiler.CompileAtRevision(migrationChanged, revision)
	if err != nil {
		t.Fatal(err)
	}
	if migrationSnapshot.Hash == first.Hash {
		t.Fatal("V2 migration gate change did not change configuration hash")
	}

	scheduledMessageChanged := stableHashV2Config("first-secret")
	scheduledMessageChanged.Providers[0].ScheduledMessage = &domain.ProviderScheduledMessage{
		Enabled: true, UpstreamModel: "model-a", Message: "scheduled health check",
	}
	scheduledMessageSnapshot, err := compiler.CompileAtRevision(scheduledMessageChanged, revision)
	if err != nil {
		t.Fatal(err)
	}
	if scheduledMessageSnapshot.Hash == first.Hash {
		t.Fatal("V2 Provider scheduled message change did not change configuration hash")
	}
}

func TestCompilerBuildsProviderModelUnionAndKeepsExecutionLeaves(t *testing.T) {
	endpoint := &domain.Endpoint{ID: "upstream", BaseURL: "https://example.test", Protocol: domain.ProtocolOpenAIResponses}
	plan := &domain.Plan{ID: "payg", Kind: domain.PlanPAYG}
	f := &config.File{
		V2DomainSource: config.V2DomainSourceNative,
		Providers: []domain.Provider{{
			ID: "provider", Enabled: true,
			Connections: []domain.ConnectionRef{{ID: "connection-a"}, {ID: "connection-b"}},
		}},
		Connections: []domain.Connection{
			{ID: "connection-a", ProviderID: "provider", Enabled: true, Models: []domain.ConnectionModel{{
				UpstreamModel: "model-x", ExecutionTargetID: "leaf-a", Capabilities: domain.Capabilities{Text: true},
			}}},
			{ID: "connection-b", ProviderID: "provider", Enabled: false, Models: []domain.ConnectionModel{{
				UpstreamModel: "model-x", ExecutionTargetID: "leaf-b", Capabilities: domain.Capabilities{Stream: true},
			}}},
		},
		Targets: []domain.ResourceTarget{
			{ID: "leaf-a", Endpoint: endpoint, Plan: plan, Model: "model-x"},
			{ID: "leaf-b", Endpoint: endpoint, Plan: plan, Model: "model-x"},
		},
	}
	snap, err := NewCompiler().Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	view := snap.ProviderModels[domain.ProviderModelKey("provider", "model-x")]
	if view == nil || len(view.SupportedConnections) != 2 || len(view.EligibleConnections) != 1 ||
		view.EligibleConnections[0] != "connection-a" || !view.Capabilities.Text || !view.Capabilities.Stream {
		t.Fatalf("provider model union = %+v", view)
	}
	if target := snap.Targets["leaf-a"]; target == nil || target.ProviderID != "provider" || target.ConnectionID != "connection-a" {
		t.Fatalf("runtime leaf owner = %+v", target)
	}
}

func TestCompilerBuildsProtocolScopedViewsForLegacyChannels(t *testing.T) {
	f := &config.File{Channels: []domain.Channel{{
		ID: "legacy", BaseURL: "https://provider.example/v1",
		Models: []domain.ChannelModel{{
			Model: "model-x",
			Protocols: []domain.Protocol{
				domain.ProtocolOpenAIChat, domain.ProtocolAnthropic,
			},
			Capabilities: domain.Capabilities{Text: true, StructuredOutput: true},
		}},
	}}}
	snapshot, err := NewCompiler().Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	target := snapshot.Targets[domain.ResourceTargetID(domain.ChannelTargetID("legacy", "model-x"))]
	if target == nil {
		t.Fatal("legacy channel target missing")
	}
	chat := target.CapabilitiesFor(domain.ProtocolOpenAIChat)
	if !chat.Text || !chat.StructuredOutput {
		t.Fatalf("OpenAI channel capability view=%+v", chat)
	}
	anthropic := target.CapabilitiesFor(domain.ProtocolAnthropic)
	if !anthropic.Text || !anthropic.StructuredOutput {
		t.Fatalf("Anthropic channel capability view=%+v", anthropic)
	}
	if !target.Capabilities.StructuredOutput {
		t.Fatalf("aggregate channel inventory lost OpenAI capability: %+v", target.Capabilities)
	}
}

func TestCompilerAppliesProtocolBaselineToClassicTargets(t *testing.T) {
	endpoint := &domain.Endpoint{
		ID: "anthropic", BaseURL: "https://provider.example/v1", Protocol: domain.ProtocolAnthropic,
	}
	f := &config.File{
		Endpoints: []domain.Endpoint{*endpoint},
		Targets: []domain.ResourceTarget{{
			ID: "classic", EndpointID: "anthropic", Endpoint: endpoint, Model: "model-x",
			Capabilities: domain.Capabilities{Text: true, StructuredOutput: true},
		}},
	}
	snapshot, err := NewCompiler().Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	target := snapshot.Targets["classic"]
	if target == nil {
		t.Fatal("classic target missing")
	}
	anthropic := target.CapabilitiesFor(domain.ProtocolAnthropic)
	if !anthropic.Text || !anthropic.StructuredOutput || !target.Capabilities.StructuredOutput {
		t.Fatalf("classic Anthropic baseline was not applied: view=%+v aggregate=%+v", anthropic, target.Capabilities)
	}
}

func TestCompilerSynthesizesPrivateLeavesFromNativeV2Connections(t *testing.T) {
	f := &config.File{
		V2DomainSource: config.V2DomainSourceNative,
		Providers: []domain.Provider{{
			ID: "provider", BaseURL: "https://example.test", Enabled: true,
			Connections: []domain.ConnectionRef{{ID: "account"}},
		}},
		Connections: []domain.Connection{{
			ID: "account", ProviderID: "provider", Enabled: true,
			Credential: domain.CredentialRef{SecretRef: "store:provider-account"},
			Models: []domain.ConnectionModel{{
				UpstreamModel: "model-x", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses},
				Capabilities: domain.Capabilities{Text: true},
			}},
		}},
	}
	snap, err := NewCompiler().Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	targetID := domain.V2ExecutionTargetID("provider", "account", "model-x")
	target := snap.Targets[targetID]
	if target == nil || target.ProviderID != "provider" || target.ConnectionID != "account" ||
		target.Model != "model-x" || target.Endpoint == nil || target.Endpoint.BaseURL != "https://example.test" {
		t.Fatalf("native execution leaf = %+v", target)
	}
	if targetID == "provider" || targetID == "account" || string(targetID) == "model-x" {
		t.Fatalf("private runtime id leaked a product object id: %s", targetID)
	}
}

func TestCompilerDoesNotInferCustomProviderCapabilitiesFromModelNames(t *testing.T) {
	f := &config.File{
		V2DomainSource: config.V2DomainSourceNative,
		Providers: []domain.Provider{
			{ID: "zhipu", Type: domain.ProviderCustom, BaseURL: "https://open.bigmodel.test/v1", Enabled: true, Connections: []domain.ConnectionRef{{ID: "zhipu-chat"}}},
			{ID: "kimi", Type: domain.ProviderCustom, BaseURL: "https://api.kimi.test/v1", Enabled: true, Connections: []domain.ConnectionRef{{ID: "kimi-chat"}}},
		},
		Connections: []domain.Connection{
			{ID: "zhipu-chat", ProviderID: "zhipu", Enabled: true, Models: []domain.ConnectionModel{{
				UpstreamModel: "glm-5.3-flash", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
				Capabilities: domain.Capabilities{Text: true, Stream: true, Tools: true},
			}}},
			{ID: "kimi-chat", ProviderID: "kimi", Enabled: true, Models: []domain.ConnectionModel{{
				UpstreamModel: "k3", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat},
				Capabilities: domain.Capabilities{Text: true, Stream: true, Tools: true},
			}}},
		},
	}
	snapshot, err := NewCompiler().Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	for _, connectionID := range []string{"zhipu-chat", "kimi-chat"} {
		var found *domain.CompiledTarget
		for _, target := range snapshot.Targets {
			if target.ConnectionID == connectionID {
				found = target
				break
			}
		}
		if found == nil || !found.Capabilities.Text || !found.Capabilities.Stream ||
			!found.Capabilities.Tools || found.Capabilities.Reasoning {
			t.Fatalf("%s runtime target capabilities = %+v", connectionID, found)
		}
	}
}

func TestCompilerUsesConnectionEndpointOverrideAndConversionPolicy(t *testing.T) {
	f := &config.File{
		V2DomainSource: config.V2DomainSourceNative,
		Providers: []domain.Provider{{
			ID: "provider", Type: domain.ProviderCustom, BaseURL: "https://default.test/v1", Enabled: true,
			ProtocolConversion: map[domain.Protocol]bool{domain.ProtocolOpenAIChat: true, domain.ProtocolAnthropic: false},
			Connections:        []domain.ConnectionRef{{ID: "responses"}},
		}},
		Connections: []domain.Connection{{
			ID: "responses", ProviderID: "provider", FullURL: "https://responses.test/custom/invoke", Enabled: true,
			Credential: domain.CredentialRef{SecretRef: "store:key"},
			Models:     []domain.ConnectionModel{{UpstreamModel: "model-x", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses}, Capabilities: domain.Capabilities{Text: true}}},
		}},
	}
	snap, err := NewCompiler().Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	target := snap.Targets[domain.V2ExecutionTargetID("provider", "responses", "model-x")]
	if target == nil || target.Endpoint.FullURL != "https://responses.test/custom/invoke" {
		t.Fatalf("target=%+v", target)
	}
	if !target.AcceptsProtocol(domain.ProtocolOpenAIChat) {
		t.Fatal("chat conversion should be enabled")
	}
	if target.AcceptsProtocol(domain.ProtocolAnthropic) {
		t.Fatal("anthropic conversion should be disabled")
	}
	if !target.AcceptsProtocol(domain.ProtocolOpenAIResponses) {
		t.Fatal("native responses must remain accepted")
	}
}

func TestCompilerForcesCodexChatAndAnthropicConversionToResponses(t *testing.T) {
	f := &config.File{
		V2DomainSource: config.V2DomainSourceNative,
		Providers: []domain.Provider{{
			ID: "codex", Type: domain.ProviderCliproxyCodex, BaseURL: domain.CodexBaseURL, Enabled: true,
			ProtocolConversion: map[domain.Protocol]bool{
				domain.ProtocolOpenAIChat: false, domain.ProtocolAnthropic: false,
			},
			Connections: []domain.ConnectionRef{{ID: "account"}},
		}},
		Connections: []domain.Connection{{
			ID: "account", ProviderID: "codex", Enabled: true,
			Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("account")},
			Models: []domain.ConnectionModel{{
				UpstreamModel: "gpt-5.5", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses},
				Capabilities: domain.Capabilities{Text: true},
			}},
		}},
	}
	snapshot, err := NewCompiler().Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	target := snapshot.Targets[domain.V2ExecutionTargetID("codex", "account", "gpt-5.5")]
	if target == nil || target.EgressProtocol(domain.ProtocolOpenAIChat) != domain.ProtocolOpenAIResponses ||
		target.EgressProtocol(domain.ProtocolAnthropic) != domain.ProtocolOpenAIResponses ||
		!target.AcceptsProtocol(domain.ProtocolOpenAIChat) || !target.AcceptsProtocol(domain.ProtocolAnthropic) {
		t.Fatalf("Codex conversion target=%+v", target)
	}
	if target.AcceptsProtocol(domain.ProtocolOpenAIImages) {
		t.Fatal("Codex OAuth target accepted image conversion")
	}
	if !target.Capabilities.Stream || !target.Capabilities.Tools || !target.Capabilities.ParallelTools || !target.Capabilities.Reasoning {
		t.Fatalf("Codex built-in capabilities were not normalized at compile time: %+v", target.Capabilities)
	}
}

func TestCompilerBuildsCodexGPTImageAsNativeImageTarget(t *testing.T) {
	f := &config.File{
		V2DomainSource: config.V2DomainSourceNative,
		Providers: []domain.Provider{{
			ID: "codex", Type: domain.ProviderCliproxyCodex, BaseURL: domain.CodexBaseURL, Enabled: true,
			Connections: []domain.ConnectionRef{{ID: "account"}},
		}},
		Connections: []domain.Connection{{
			ID: "account", ProviderID: "codex", Enabled: true,
			Credential: domain.CredentialRef{SecretRef: domain.OAuthRef("account")},
			Models: []domain.ConnectionModel{{
				UpstreamModel: "gpt-image-2", Protocols: []domain.Protocol{domain.ProtocolOpenAIResponses},
			}},
		}},
	}
	snapshot, err := NewCompiler().Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	target := snapshot.Targets[domain.V2ExecutionTargetID("codex", "account", "gpt-image-2")]
	if target == nil || !target.AcceptsProtocol(domain.ProtocolOpenAIImages) ||
		target.AcceptsProtocol(domain.ProtocolOpenAIResponses) ||
		target.Endpoint.Protocol != domain.ProtocolOpenAIImages ||
		!target.Capabilities.Images || target.Capabilities.Text || target.Capabilities.Stream {
		t.Fatalf("Codex image target=%+v", target)
	}
}

func TestCompilerAppliesProtocolScopedCapabilityEvidenceAndPreservesLimits(t *testing.T) {
	const baseURL = "https://provider.example/v1"
	configured := domain.Capabilities{
		Text: true, Stream: false, Tools: false, ParallelTools: true, Reasoning: false,
		StructuredOutput: true, Vision: false, ContextWindow: 131072, MaxOutput: 8192,
	}
	f := &config.File{
		V2DomainSource: config.V2DomainSourceNative,
		Providers: []domain.Provider{{
			ID: "provider", Type: domain.ProviderCustom, BaseURL: baseURL, Enabled: true,
			Connections: []domain.ConnectionRef{{ID: "account"}},
		}},
		Connections: []domain.Connection{{
			ID: "account", ProviderID: "provider", Enabled: true,
			Models: []domain.ConnectionModel{{
				UpstreamModel: "model-x",
				Protocols: []domain.Protocol{
					domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic,
				},
				Capabilities: configured,
			}},
		}},
	}
	type observation struct {
		supported bool
		found     bool
	}
	evidence := map[domain.Protocol]map[string]observation{
		domain.ProtocolAnthropic: {
			domain.CapabilityStructuredOutput: {supported: false, found: true},
		},
		domain.ProtocolOpenAIChat: {
			domain.CapabilityStream:           {supported: true, found: true},
			domain.CapabilityParallelTools:    {supported: false, found: true},
			domain.CapabilityReasoning:        {supported: true, found: true},
			domain.CapabilityStructuredOutput: {supported: false, found: true},
			domain.CapabilityVision:           {supported: true, found: true},
		},
		domain.ProtocolOpenAIResponses: {
			domain.CapabilityStream:           {supported: false, found: true},
			domain.CapabilityTools:            {supported: true, found: true},
			domain.CapabilityParallelTools:    {supported: true, found: true},
			domain.CapabilityReasoning:        {supported: false, found: true},
			domain.CapabilityStructuredOutput: {supported: true, found: true},
			domain.CapabilityVision:           {supported: false, found: true},
		},
	}
	compiler := NewCompilerWithCapabilityEvidence(func(providerID, endpointFingerprint, model string,
		protocol domain.Protocol, capability string, _ time.Time) (bool, bool, error) {
		if providerID != "provider" || model != "model-x" {
			t.Fatalf("unexpected evidence identity provider=%q model=%q", providerID, model)
		}
		endpoint, err := domain.ProviderProtocolEndpointURL(baseURL, "", protocol)
		if err != nil {
			t.Fatal(err)
		}
		wantFingerprint := domain.ProviderCapabilityEndpointFingerprint(domain.ProviderCustom, endpoint)
		if endpointFingerprint == domain.ConnectionCapabilityFingerprint(wantFingerprint, &f.Connections[0]) {
			return false, false, nil // Legacy evidence remains a fallback.
		}
		if endpointFingerprint != wantFingerprint {
			t.Fatalf("endpoint fingerprint=%q want %q for %s", endpointFingerprint, wantFingerprint, protocol)
		}
		item, ok := evidence[protocol][capability]
		if !ok {
			return false, false, nil
		}
		return item.supported, item.found, nil
	})

	snapshot, err := compiler.Compile(f)
	if err != nil {
		t.Fatal(err)
	}
	target := snapshot.Targets[domain.V2ExecutionTargetID("provider", "account", "model-x")]
	if target == nil {
		t.Fatal("compiled target missing")
	}
	chatUpstream := target.UpstreamCapabilitiesFor(domain.ProtocolOpenAIChat)
	if !chatUpstream.Text || !chatUpstream.Stream || chatUpstream.Tools || chatUpstream.ParallelTools || !chatUpstream.Reasoning ||
		chatUpstream.StructuredOutput || !chatUpstream.Vision {
		t.Fatalf("chat upstream capability evidence not applied: %+v", chatUpstream)
	}
	chat := target.CapabilitiesFor(domain.ProtocolOpenAIChat)
	if chat.ParallelTools {
		t.Fatalf("chat exposed parallel_tools without tools: %+v", chat)
	}
	responses := target.CapabilitiesFor(domain.ProtocolOpenAIResponses)
	if !responses.Text || responses.Stream || !responses.Tools || !responses.ParallelTools || responses.Reasoning ||
		!responses.StructuredOutput || responses.Vision {
		t.Fatalf("responses capability evidence not isolated: %+v", responses)
	}
	anthropicUpstream := target.UpstreamCapabilitiesFor(domain.ProtocolAnthropic)
	if !anthropicUpstream.Text || anthropicUpstream.Stream || anthropicUpstream.Tools || !anthropicUpstream.ParallelTools || anthropicUpstream.Reasoning ||
		anthropicUpstream.StructuredOutput || anthropicUpstream.Vision {
		t.Fatalf("anthropic upstream baseline or missing-evidence fallback changed: %+v", anthropicUpstream)
	}
	if anthropic := target.CapabilitiesFor(domain.ProtocolAnthropic); anthropic.ParallelTools {
		t.Fatalf("anthropic exposed parallel_tools without tools: %+v", anthropic)
	}
	for protocol, capabilities := range target.ProtocolCapabilities {
		if capabilities.ContextWindow != configured.ContextWindow || capabilities.MaxOutput != configured.MaxOutput {
			t.Fatalf("%s numeric limits were changed by boolean evidence: %+v", protocol, capabilities)
		}
	}
	if !target.Capabilities.Text || !target.Capabilities.Stream || !target.Capabilities.Tools ||
		!target.Capabilities.ParallelTools || !target.Capabilities.Reasoning ||
		!target.Capabilities.StructuredOutput || !target.Capabilities.Vision ||
		target.Capabilities.ContextWindow != configured.ContextWindow || target.Capabilities.MaxOutput != configured.MaxOutput {
		t.Fatalf("aggregate capability union incorrect: %+v", target.Capabilities)
	}
	catalog := snapshot.ProviderModels[domain.ProviderModelKey("provider", "model-x")]
	if catalog == nil || !catalog.Capabilities.Tools || !catalog.Capabilities.ParallelTools {
		t.Fatalf("provider model catalog did not expose effective capabilities: %+v", catalog)
	}
}

func TestCompilerCapabilityEvidenceIsolatedByProviderEndpointModelProtocolAndCapability(t *testing.T) {
	providers := []domain.Provider{
		{ID: "provider-a", Type: domain.ProviderCustom, BaseURL: "https://a.example/v1", Enabled: true, Connections: []domain.ConnectionRef{{ID: "account-a"}}},
		{ID: "provider-b", Type: domain.ProviderCustom, BaseURL: "https://b.example/v1", Enabled: true, Connections: []domain.ConnectionRef{{ID: "account-b"}}},
	}
	connections := []domain.Connection{
		{ID: "account-a", ProviderID: "provider-a", BaseURL: "https://a-account.example/v1", Enabled: true, Models: []domain.ConnectionModel{
			{UpstreamModel: "model-a", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses}, Capabilities: domain.Capabilities{Text: true}},
			{UpstreamModel: "model-b", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat}, Capabilities: domain.Capabilities{Text: true}},
		}},
		{ID: "account-b", ProviderID: "provider-b", Enabled: true, Models: []domain.ConnectionModel{
			{UpstreamModel: "model-a", Protocols: []domain.Protocol{domain.ProtocolOpenAIChat}, Capabilities: domain.Capabilities{Text: true}},
		}},
	}
	endpoint, err := domain.ProviderProtocolEndpointURL(connections[0].BaseURL, "", domain.ProtocolOpenAIChat)
	if err != nil {
		t.Fatal(err)
	}
	exact := capabilityEvidenceTestKey{
		providerID:          "provider-a",
		endpointFingerprint: domain.ProviderCapabilityEndpointFingerprint(domain.ProviderCustom, endpoint),
		model:               "model-a", protocol: domain.ProtocolOpenAIChat, capability: domain.CapabilityTools,
	}
	compiler := NewCompilerWithCapabilityEvidence(func(providerID, endpointFingerprint, model string,
		protocol domain.Protocol, capability string, _ time.Time) (bool, bool, error) {
		key := capabilityEvidenceTestKey{providerID, endpointFingerprint, model, protocol, capability}
		return key == exact, key == exact, nil
	})
	snapshot, err := compiler.Compile(&config.File{
		V2DomainSource: config.V2DomainSourceNative, Providers: providers, Connections: connections,
	})
	if err != nil {
		t.Fatal(err)
	}
	lookup := func(providerID, connectionID, model string, protocol domain.Protocol) domain.Capabilities {
		t.Helper()
		target := snapshot.Targets[domain.V2ExecutionTargetID(providerID, connectionID, model)]
		if target == nil {
			t.Fatalf("missing target %s/%s/%s", providerID, connectionID, model)
		}
		return target.CapabilitiesFor(protocol)
	}
	if !lookup("provider-a", "account-a", "model-a", domain.ProtocolOpenAIChat).Tools {
		t.Fatal("exact five-dimensional evidence did not apply")
	}
	if lookup("provider-a", "account-a", "model-a", domain.ProtocolOpenAIResponses).Tools {
		t.Fatal("chat evidence leaked into responses protocol")
	}
	if lookup("provider-a", "account-a", "model-b", domain.ProtocolOpenAIChat).Tools {
		t.Fatal("model-a evidence leaked into model-b")
	}
	if lookup("provider-b", "account-b", "model-a", domain.ProtocolOpenAIChat).Tools {
		t.Fatal("provider-a endpoint evidence leaked into provider-b")
	}
}
