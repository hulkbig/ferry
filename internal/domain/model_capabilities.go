package domain

// NormalizeProviderModelCapabilities applies protocol defaults to operator or
// probe-backed declarations. Custom upstream model names are opaque: their
// feature support must come from exact endpoint evidence or authoritative
// configuration, never from name-family guesses.
//
// Codex keeps its authoritative replacement behavior because Ferry owns
// that model catalog and wire protocol.
func NormalizeProviderModelCapabilities(providerType ProviderType, model string, protocols []Protocol, declared Capabilities) Capabilities {
	if providerType == ProviderCliproxyCodex {
		if capabilities, known := CodexModelCapabilities(model); known {
			// Account entitlements cannot be supplied by a static model preset.
			capabilities.Priority = declared.Priority && capabilities.Text
			return capabilities
		}
	}
	return NormalizeProtocolCapabilities(declared, protocols)
}
