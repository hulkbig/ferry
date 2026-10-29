// Package credentialimport normalizes explicit operator uploads. It never reads
// local paths, fetches remote URLs, or returns credential material in previews.
package credentialimport

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode"

	"ferry/internal/domain"
	"ferry/internal/nativeauth"
	"ferry/internal/oauth"
	"gopkg.in/yaml.v3"
)

const MaxBytes = 4 << 20
const MaxItems = 500

var versionSegment = regexp.MustCompile(`(?i)^v[0-9]+(\.[0-9]+|(alpha|beta|preview).*)?$`)
var namespacePattern = regexp.MustCompile(`^[a-z][a-z0-9-]{0,23}$`)

type Request struct {
	Source    string            `json:"source"`
	Namespace string            `json:"namespace"`
	Document  string            `json:"document"`
	AuthFiles []json.RawMessage `json:"auth_files,omitempty"`
	// ClientKeys is an explicit supplement; sub2api account exports omit these.
	ClientKeys []ClientKey `json:"client_keys,omitempty"`
	Selected   []string    `json:"selected,omitempty"`
}
type ClientKey struct {
	Name string `json:"name"`
	Key  string `json:"key"`
}
type Item struct {
	ID           string              `json:"id"`
	Name         string              `json:"name"`
	Kind         string              `json:"kind"`
	ProviderType domain.ProviderType `json:"provider_type,omitempty"`
	BaseURL      string              `json:"base_url,omitempty"`
	Header       string              `json:"header,omitempty"`
	Prefix       string              `json:"prefix,omitempty"`
	Protocol     domain.Protocol     `json:"protocol,omitempty"`
	Models       []string            `json:"models,omitempty"`
	Warnings     []string            `json:"warnings,omitempty"`
	Error        string              `json:"error,omitempty"`
	Secret       string              `json:"-"`
	Auth         json.RawMessage     `json:"-"`
	Native       *nativeauth.Token   `json:"-"`
}
type Plan struct {
	Items    []Item   `json:"items"`
	Warnings []string `json:"warnings"`
}

func text(m map[string]any, k string) string { s, _ := m[k].(string); return strings.TrimSpace(s) }
func object(v any) map[string]any            { m, _ := v.(map[string]any); return m }
func list(v any) []any                       { a, _ := v.([]any); return a }
func validSecret(s string) bool {
	return s != "" && len(s) <= 64<<10 && !strings.ContainsAny(s, "\r\n") && !strings.Contains(s, "***") && !strings.Contains(s, "••")
}
func safeURL(s string) bool {
	u, e := url.Parse(s)
	return e == nil && (u.Scheme == "https" || u.Scheme == "http") && u.Host != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}
func identity(namespace, source, path string) string {
	h := sha256.Sum256([]byte(source + ":" + path))
	return "imp-" + namespace + "-" + hex.EncodeToString(h[:8])
}

func Parse(r Request) (Plan, error) {
	p := Plan{Items: []Item{}, Warnings: []string{"All imported providers and client keys are disabled. Review models, endpoint, access policy and limits before enabling. Source routing, aliases, proxies, quotas, billing and group permissions are not migrated."}}
	if !namespacePattern.MatchString(r.Namespace) {
		return p, errors.New("namespace must be 1–24 lowercase letters, digits or hyphens, starting with a letter")
	}
	if r.Source != "sub2api" && r.Source != "cliproxyapi" {
		return p, errors.New("source must be sub2api or cliproxyapi")
	}
	size := len(r.Document)
	for _, raw := range r.AuthFiles {
		size += len(raw)
	}
	for _, k := range r.ClientKeys {
		size += len(k.Name) + len(k.Key)
	}
	if size > MaxBytes {
		return p, errors.New("upload exceeds 4 MiB")
	}
	add := func(path string, item Item) {
		item.ID = identity(r.Namespace, r.Source, path)
		if item.Name == "" {
			item.Name = path
		}
		if len(item.Name) > 160 {
			item.Name = item.Name[:160]
		}
		p.Items = append(p.Items, item)
	}
	if strings.TrimSpace(r.Document) != "" {
		var root map[string]any
		if yaml.Unmarshal([]byte(r.Document), &root) != nil || root == nil {
			return p, errors.New("invalid JSON/YAML object")
		}
		if r.Source == "sub2api" {
			if d := object(root["data"]); d != nil {
				root = d
			}
			if v, ok := root["version"]; ok && fmt.Sprint(v) != "1" {
				return p, errors.New("unsupported sub2api export version")
			}
			accounts, ok := root["accounts"].([]any)
			if !ok {
				return p, errors.New("sub2api export must contain an accounts array")
			}
			for i, v := range accounts {
				a := object(v)
				c := object(a["credentials"])
				platform := text(a, "platform")
				kind := text(a, "type")
				item := Item{Name: text(a, "name")}
				switch {
				case kind == "apikey":
					item = apiKey(c, platform, r.Source)
					if platform == "anthropic" && text(object(a["extra"]), "anthropic_apikey_auth_scheme") == "authorization_bearer" {
						item.Header = "Authorization"
						item.Prefix = "Bearer "
					}
					item.Name = text(a, "name")
				case kind == "oauth" && platform == "openai":
					c = copyMap(c)
					c["type"] = "codex"
					if text(c, "account_id") == "" {
						c["account_id"] = text(c, "chatgpt_account_id")
					}
					if c["expired"] == nil {
						c["expired"] = expiry(c["expires_at"])
					}
					item = auth(c)
					item.Name = text(a, "name")
				case kind == "oauth" && platform == "anthropic":
					c = copyMap(c)
					c["type"] = "claude"
					c["expired"] = expiry(c["expires_at"])
					item = auth(c)
					item.Name = text(a, "name")
				default:
					item.Error = "Unsupported account type/platform; no credentials will be written"
				}
				if text(a, "proxy_key") != "" {
					item.Warnings = append(item.Warnings, "Source proxy is not migrated; configure egress before enabling")
				}
				if len(object(c["model_mapping"])) > 0 {
					item.Warnings = append(item.Warnings, "Model mappings require manual review")
				}
				add(fmt.Sprintf("accounts/%d", i), item)
			}
		} else {
			if text(root, "type") != "" && root["access_token"] != nil {
				add("auth/0", auth(root))
			} else {
				for i, v := range list(root["api-keys"]) {
					add(fmt.Sprintf("client/%d", i), clientItem(fmt.Sprintf("Client key %d", i+1), v))
				}
				for i, v := range list(object(root["access"])["api-keys"]) {
					add(fmt.Sprintf("access/%d", i), clientItem(fmt.Sprintf("Client key %d", i+1), v))
				}
				groups := object(root["api-keys"])
				kinds := []string{"openai-compatibility", "claude", "codex", "gemini", "vertex", "xai"}
				known := map[string]bool{}
				for _, kind := range kinds {
					known[kind] = true
				}
				extra := []string{}
				for kind := range groups {
					if !known[kind] {
						extra = append(extra, kind)
					}
				}
				sort.Strings(extra)
				kinds = append(kinds, extra...)
				for _, kind := range kinds {
					legacy := kind + "-api-key"
					if kind == "openai-compatibility" {
						legacy = kind
					}
					for _, source := range []struct {
						path    string
						entries []any
					}{{legacy, list(root[legacy])}, {"api-keys/" + kind, list(groups[kind])}} {
						for i, v := range source.entries {
							g := object(v)
							keys := list(g["keys"])
							if keys == nil {
								keys = list(g["api-key-entries"])
							}
							if keys == nil {
								keys = []any{g}
							}
							for j, k := range keys {
								merged := copyMap(g)
								for key, value := range object(k) {
									merged[key] = value
								}
								item := apiKey(merged, kind, r.Source)
								item.Name = text(g, "name")
								add(fmt.Sprintf("%s/%d/%d", source.path, i, j), item)
							}
						}
					}
				}
			}
		}
	}
	for i, raw := range r.AuthFiles {
		var m map[string]any
		if json.Unmarshal(raw, &m) != nil || m == nil {
			add(fmt.Sprintf("auth-files/%d", i), Item{Error: "Invalid auth JSON"})
		} else {
			add(fmt.Sprintf("auth-files/%d", i), auth(m))
		}
	}
	for i, k := range r.ClientKeys {
		add(fmt.Sprintf("client-keys/%d", i), clientItem(k.Name, k.Key))
	}
	if len(p.Items) == 0 {
		return p, errors.New("no recognized accounts, upstream keys or client keys found")
	}
	if len(p.Items) > MaxItems {
		return p, errors.New("upload exceeds 500 items; split the export")
	}
	return p, nil
}
func copyMap(m map[string]any) map[string]any {
	out := map[string]any{}
	for k, v := range m {
		out[k] = v
	}
	return out
}
func expiry(v any) string {
	switch x := v.(type) {
	case string:
		if n, e := strconv.ParseInt(x, 10, 64); e == nil {
			return time.Unix(n, 0).UTC().Format(time.RFC3339)
		}
		return x
	case int:
		return time.Unix(int64(x), 0).UTC().Format(time.RFC3339)
	case float64:
		return time.Unix(int64(x), 0).UTC().Format(time.RFC3339)
	}
	return ""
}
func clientItem(name string, v any) Item {
	s, _ := v.(string)
	i := Item{Name: name, Kind: "client_key", Secret: strings.TrimSpace(s), Warnings: []string{"Source key permissions, expiration and billing limits are not transferred; configure these before enabling"}}
	if !validSecret(i.Secret) {
		i.Error = "Missing, masked or invalid client key"
	}
	return i
}
func apiKey(m map[string]any, platform, source string) Item {
	i := Item{Kind: "api_key", ProviderType: domain.ProviderCustom, Protocol: domain.ProtocolOpenAIChat, BaseURL: text(m, "base_url"), Secret: text(m, "api_key")}
	if i.BaseURL == "" {
		i.BaseURL = text(m, "base-url")
	}
	if i.Secret == "" {
		i.Secret = text(m, "api-key")
	}
	switch platform {
	case "anthropic", "claude":
		i.Protocol = domain.ProtocolAnthropic
		if i.BaseURL == "" {
			i.BaseURL = "https://api.anthropic.com/v1"
		}
	case "openai":
		if i.BaseURL == "" {
			i.BaseURL = "https://api.openai.com/v1"
		}
	case "codex":
		i.Protocol = domain.ProtocolOpenAIResponses
		// Require an explicit endpoint; Codex proxy keys need not target OpenAI PAYG.
	case "openai-compatibility":
	case "xai":
		if i.BaseURL == "" {
			i.BaseURL = "https://api.x.ai/v1"
		}
	default:
		i.Error = "Native Gemini/Vertex or unknown API-key protocol is not supported by this importer"
	}
	if !validSecret(i.Secret) {
		i.Error = "Missing, masked or invalid upstream API key"
	}
	if !safeURL(i.BaseURL) {
		i.Error = "An absolute HTTP(S) base URL without embedded credentials, query or fragment is required"
		i.BaseURL = ""
	}
	if i.BaseURL != "" {
		i.Header = "Authorization"
		i.Prefix = "Bearer "
		u, _ := url.Parse(i.BaseURL)
		provided := text(m, "base_url") != "" || text(m, "base-url") != ""
		if i.Protocol == domain.ProtocolAnthropic {
			i.Header = "x-api-key"
			i.Prefix = ""
			if provided {
				i.BaseURL = strings.TrimRight(i.BaseURL, "/") + "/v1"
			}
			if (source == "cliproxyapi" && !strings.EqualFold(u.Hostname(), "api.anthropic.com")) || (source == "sub2api" && strings.EqualFold(u.Hostname(), "ollama.com")) {
				i.Header = "Authorization"
				i.Prefix = "Bearer "
			}
		} else if source == "sub2api" {
			base := strings.TrimRight(i.BaseURL, "/")
			if strings.HasSuffix(base, "/chat/completions") {
				base = strings.TrimSuffix(base, "/chat/completions")
			} else if strings.HasSuffix(base, "/responses") {
				base = strings.TrimSuffix(base, "/responses")
				i.Protocol = domain.ProtocolOpenAIResponses
			} else {
				path := strings.TrimRight(u.Path, "/")
				segment := path[strings.LastIndex(path, "/")+1:]
				if !versionSegment.MatchString(segment) {
					base += "/v1"
				}
			}
			i.BaseURL = base
		}
	}
	seen := map[string]bool{}
	for _, v := range list(m["models"]) {
		name := text(object(v), "name")
		if name == "" {
			name, _ = v.(string)
		}
		if name != "" && strings.IndexFunc(name, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) >= 0 {
			i.Error = "Invalid upstream model name"
			continue
		}
		if name != "" && !seen[name] {
			i.Models = append(i.Models, name)
			seen[name] = true
		}
	}
	sort.Strings(i.Models)
	if len(i.Models) == 0 {
		i.Warnings = append(i.Warnings, "No explicit models; discover and verify models before enabling")
	}
	for _, k := range []string{"proxy-url", "headers", "prefix", "excluded-models", "model_mapping"} {
		if m[k] != nil {
			i.Warnings = append(i.Warnings, "Source "+k+" is not migrated")
		}
	}
	return i
}
func auth(m map[string]any) Item {
	i := Item{Name: text(m, "email"), Kind: text(m, "type"), ProviderType: domain.ProviderCustom}
	switch i.Kind {
	case "codex":
		raw, _ := json.Marshal(m)
		if _, e := oauth.ParseAuthJSON(raw, time.Now()); e != nil {
			i.Error = "Invalid or unsupported Codex OAuth credential (requires access/refresh tokens and a supported OAuth client; SIWC is unsupported)"
			return i
		}
		i.Auth = raw
		i.BaseURL = domain.CodexBaseURL
		i.ProviderType = domain.ProviderCliproxyCodex
		i.Protocol = domain.ProtocolOpenAIResponses
	case "claude":
		if text(m, "client_id") != "" && text(m, "client_id") != "9d1c250a-e61b-44d9-88ed-5944d1962f5e" {
			i.Error = "Unsupported Claude OAuth client"
			return i
		}
		token := nativeauth.Token{AccessToken: text(m, "access_token"), RefreshToken: text(m, "refresh_token"), Identity: text(m, "email")}
		if !validSecret(token.AccessToken) || !validSecret(token.RefreshToken) {
			i.Error = "Claude OAuth requires unmasked access and refresh tokens"
			return i
		}
		token.ExpiresAt, _ = time.Parse(time.RFC3339, expiry(m["expired"]))
		if token.ExpiresAt.IsZero() {
			token.ExpiresAt = time.Now().Add(-time.Second)
		}
		i.Native = &token
		i.BaseURL = "https://api.anthropic.com/v1"
		i.Protocol = domain.ProtocolAnthropic
	default:
		i.Error = "Unsupported OAuth provider; original file remains unchanged"
	}
	i.Warnings = append(i.Warnings, "OAuth credentials are stored without contacting the provider; validity and entitlement require later verification")
	return i
}
