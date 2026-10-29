package domain

// Built-in OAuth (Phase 4, CLIProxyAPI feature): ChatGPT/Codex subscription
// accounts managed by the gateway itself. Their credentials never live in
// config files — configs carry only an opaque ref, and the OAuth manager
// materializes a live access token at publish/request time.

// OAuthRefPrefix marks credential refs resolved through the built-in OAuth
// manager: oauth:codex:<account>.
const OAuthRefPrefix = "oauth:codex:"

// OAuthRef builds the credential ref addressing a managed Codex account.
func OAuthRef(account string) string { return OAuthRefPrefix + account }

// validRefName mirrors the config-side id charset ([a-zA-Z0-9][a-zA-Z0-9_-]*)
// without importing the config package (which depends on this one).
func validRefName(s string) bool {
	if s == "" || len(s) > 64 {
		return false
	}
	for i, c := range s {
		ok := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' ||
			(i > 0 && (c == '_' || c == '-'))
		if !ok {
			return false
		}
	}
	return true
}

// OAuthAccountFromRef splits oauth:codex:<account> into its account name.
func OAuthAccountFromRef(ref string) (string, bool) {
	if len(ref) <= len(OAuthRefPrefix) || ref[:len(OAuthRefPrefix)] != OAuthRefPrefix {
		return "", false
	}
	acct := ref[len(OAuthRefPrefix):]
	if !validRefName(acct) {
		return "", false
	}
	return acct, true
}

// OAuthAccountStatus is the redacted view of one managed OAuth account:
// identity and freshness only — token material never leaves the manager.
type OAuthAccountStatus struct {
	Name      string `json:"name"`
	Email     string `json:"email,omitempty"`
	PlanType  string `json:"plan_type,omitempty"`
	AccountID string `json:"account_id,omitempty"`
	ExpiresAt string `json:"expires_at,omitempty"` // RFC3339
	Expired   bool   `json:"expired"`
}
