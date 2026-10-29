package domain

// ---------------------------------------------------------------------------
// Channels model generic upstreams — one base URL + credential serving many
// models, each declaring which wire protocols it natively speaks. The compiler
// expands channel×model into ordinary ResourceTargets so scheduling, ranking,
// quota, health, and recovery all reuse the existing machinery untouched.

// ChannelType distinguishes plain HTTP upstreams from built-in account types.
type ChannelType string

const (
	// ChannelCustom is any OpenAI/Anthropic-compatible HTTP upstream.
	ChannelCustom ChannelType = "custom"
	// ChannelCliproxyCodex is a ChatGPT/Codex subscription account managed
	// by the built-in OAuth flow (CLIProxyAPI feature, Phase 1 scope).
	ChannelCliproxyCodex ChannelType = "cliproxy-codex"
)

// CodexBaseURL is the upstream served by cliproxy-codex channels.
const CodexBaseURL = "https://chatgpt.com/backend-api/codex"

// Channel is one configured upstream account/endpoint.
type Channel struct {
	ID         string         `yaml:"id"`
	Type       ChannelType    `yaml:"type"`       // default custom
	BaseURL    string         `yaml:"base_url"`   // required for custom; defaulted for cliproxy types
	Credential CredentialRef  `yaml:"credential"` // custom: required; oauth types: left empty
	Enabled    *bool          `yaml:"enabled"`
	Pools      []string       `yaml:"pools"`   // expanded targets auto-join these pools
	PlanID     string         `yaml:"plan_id"` // default plan for models without their own
	Models     []ChannelModel `yaml:"models"`
}

// TargetID returns the resource target id the compiler synthesizes for a
// channel model: "<channel>.<model>". Dots never appear in hand-written
// resource_target ids (name charset excludes them), so the two namespaces
// cannot collide.
func ChannelTargetID(channelID, model string) string {
	return channelID + "." + model
}

// Enabled reports whether the channel participates in compilation.
func (c *Channel) EnabledOrDefault() bool {
	return c.Enabled == nil || *c.Enabled
}

// ChannelModel declares one upstream model and its natively spoken formats.
// At least one protocol is mandatory: anything else is reached by automatic
// canonical-contract mapping through the gateway.
type ChannelModel struct {
	Model        string       `yaml:"model"`     // upstream model name
	Alias        string       `yaml:"alias"`     // optional client-facing name override
	Protocols    []Protocol   `yaml:"protocols"` // native formats, ≥1 enforced
	PlanID       string       `yaml:"plan_id"`   // overrides channel-level when set
	Capabilities Capabilities `yaml:"capabilities"`
	Weight       int          `yaml:"weight"`
	Priority     int          `yaml:"priority"`
	Pools        []string     `yaml:"pools"` // non-empty overrides channel-level; empty inherits
}

// PlanRef resolves the model's plan: per-model plan_id wins, else the
// channel-level default.
func (m *ChannelModel) PlanRef(channel *Channel) string {
	if m.PlanID != "" {
		return m.PlanID
	}
	return channel.PlanID
}

// ClientFacing returns the name clients use to request this model.
func (m *ChannelModel) ClientFacing() string {
	if m.Alias != "" {
		return m.Alias
	}
	return m.Model
}

// ValidProtocol reports whether p is one of the supported wire formats.
func ValidProtocol(p Protocol) bool {
	switch p {
	case ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolOpenAIImages, ProtocolAnthropic:
		return true
	}
	return false
}

// PreferredProtocol picks the least-lossy egress format from natives for a
// request that arrived as ingress but cannot be served natively. Order:
// openai-responses → openai-chat → anthropic (richest canonical coverage
// first). Falls back to the first native entry.
func PreferredProtocol(natives []Protocol) Protocol {
	for _, want := range []Protocol{ProtocolOpenAIResponses, ProtocolOpenAIChat, ProtocolAnthropic, ProtocolOpenAIImages} {
		for _, p := range natives {
			if p == want {
				return p
			}
		}
	}
	if len(natives) > 0 {
		return natives[0]
	}
	return ""
}

// HasProtocol reports whether natives contains p.
func HasProtocol(natives []Protocol, p Protocol) bool {
	for _, n := range natives {
		if n == p {
			return true
		}
	}
	return false
}
