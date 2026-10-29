package domain

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
)

// RoutingPolicySpec is one independently versioned WHEN -> route rule. Active
// rules are evaluated in ascending Priority order and the first match wins.
// Name, Description, Priority, Enabled and Revision are control-plane metadata;
// they intentionally do not participate in the semantic content hash.
type RoutingPolicySpec struct {
	ID          string             `json:"id" yaml:"id"`
	Name        string             `json:"name" yaml:"name"`
	Description string             `json:"description,omitempty" yaml:"description,omitempty"`
	Priority    int                `json:"priority" yaml:"priority"`
	Enabled     bool               `json:"enabled" yaml:"enabled"`
	Revision    int64              `json:"revision" yaml:"revision"`
	ContentHash string             `json:"content_hash,omitempty" yaml:"content_hash,omitempty"`
	Match       RoutingPolicyMatch `json:"match" yaml:"match"`
	Route       RoutingPolicyRoute `json:"route" yaml:"route"`
}

// RoutingPolicyMatch is the low-code WHEN side of a routing rule. Empty means
// catch-all. Slice fields are sets, not precedence-bearing lists.
type RoutingPolicyMatch struct {
	Consumers            []string    `json:"consumers,omitempty" yaml:"consumers,omitempty"`
	Models               []string    `json:"models,omitempty" yaml:"models,omitempty"`
	Protocols            []Protocol  `json:"protocols,omitempty" yaml:"protocols,omitempty"`
	RequiredCapabilities []string    `json:"required_capabilities,omitempty" yaml:"required_capabilities,omitempty"`
	Weekdays             []string    `json:"weekdays,omitempty" yaml:"weekdays,omitempty"`
	TimeWindow           *TimeWindow `json:"time_window,omitempty" yaml:"time_window,omitempty"`
	Timezone             string      `json:"timezone,omitempty" yaml:"timezone,omitempty"`
	Expression           string      `json:"expression,omitempty" yaml:"expression,omitempty"`
}

func (m RoutingPolicyMatch) IsCatchAll() bool {
	return len(m.Consumers) == 0 && len(m.Models) == 0 && len(m.Protocols) == 0 &&
		len(m.RequiredCapabilities) == 0 && len(m.Weekdays) == 0 &&
		m.TimeWindow == nil && strings.TrimSpace(m.Expression) == ""
}

type RoutingPolicyRoute struct {
	Steps []RoutingPolicyStep `json:"steps" yaml:"steps"`
}

// RoutingPolicyStep is one route attempt group. Exactly one of Channel or
// ChannelGroup is set. RecoverOn is typed and fail-closed; authentication and
// request-shape errors may never be converted into fallback traffic.
type RoutingPolicyStep struct {
	Channel      string        `json:"channel,omitempty" yaml:"channel,omitempty"`
	ChannelGroup string        `json:"channel_group,omitempty" yaml:"channel_group,omitempty"`
	Objective    string        `json:"objective,omitempty" yaml:"objective,omitempty"`
	RecoverOn    []FailureType `json:"recover_on,omitempty" yaml:"recover_on,omitempty"`
}

// RoutingPolicyRevision is one immutable semantic revision stored in SQLite.
type RoutingPolicyRevision struct {
	PolicyID    string             `json:"policy_id"`
	Revision    int64              `json:"revision"`
	ContentHash string             `json:"content_hash"`
	Match       RoutingPolicyMatch `json:"match"`
	Route       RoutingPolicyRoute `json:"route"`
	CreatedAt   time.Time          `json:"created_at"`
}

type routingPolicySemantic struct {
	Match RoutingPolicyMatch `json:"match"`
	Route RoutingPolicyRoute `json:"route"`
}

// RoutingPolicyContentHash returns a full SHA-256 over canonical semantic
// content. It never includes timestamps, revisions, UI labels, enablement or
// ordering metadata, allowing compiled artifacts to be safely reused.
func RoutingPolicyContentHash(spec RoutingPolicySpec) (string, error) {
	semantic := routingPolicySemantic{Match: spec.Match, Route: spec.Route}
	canonicalizeRoutingPolicy(&semantic)
	raw, err := json.Marshal(semantic)
	if err != nil {
		return "", fmt.Errorf("encode routing policy semantics: %w", err)
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalizeRoutingPolicy(semantic *routingPolicySemantic) {
	sortUniqueStrings := func(values []string) []string {
		out := append([]string(nil), values...)
		slices.Sort(out)
		return slices.Compact(out)
	}
	semantic.Match.Consumers = sortUniqueStrings(semantic.Match.Consumers)
	semantic.Match.Models = sortUniqueStrings(semantic.Match.Models)
	semantic.Match.RequiredCapabilities = sortUniqueStrings(semantic.Match.RequiredCapabilities)
	semantic.Match.Weekdays = sortUniqueStrings(semantic.Match.Weekdays)
	protocols := append([]Protocol(nil), semantic.Match.Protocols...)
	slices.Sort(protocols)
	semantic.Match.Protocols = slices.Compact(protocols)
	semantic.Match.Expression = strings.TrimSpace(semantic.Match.Expression)
	for i := range semantic.Route.Steps {
		recoverOn := append([]FailureType(nil), semantic.Route.Steps[i].RecoverOn...)
		slices.Sort(recoverOn)
		semantic.Route.Steps[i].RecoverOn = slices.Compact(recoverOn)
	}
}

// ValidateRoutingPolicies validates the ordered rule set independently from
// resource references. Cross-resource validation is performed by config.File.
func ValidateRoutingPolicies(specs []RoutingPolicySpec) error {
	seenID := map[string]bool{}
	seenPriority := map[int]bool{}
	previousPriority := -1
	enabledSeen := 0
	catchAllSeen := false
	for i, spec := range specs {
		if spec.ID == "" {
			return fmt.Errorf("routing policy %d: id required", i+1)
		}
		if seenID[spec.ID] {
			return fmt.Errorf("duplicate routing policy id %q", spec.ID)
		}
		seenID[spec.ID] = true
		if spec.Priority < 0 {
			return fmt.Errorf("routing policy %s: priority must be non-negative", spec.ID)
		}
		if seenPriority[spec.Priority] {
			return fmt.Errorf("duplicate routing policy priority %d", spec.Priority)
		}
		seenPriority[spec.Priority] = true
		if i > 0 && spec.Priority <= previousPriority {
			return fmt.Errorf("routing policies must be stored in ascending priority order")
		}
		previousPriority = spec.Priority
		if err := validateRoutingPolicyMatch(spec.ID, spec.Match); err != nil {
			return err
		}
		if len(spec.Route.Steps) == 0 {
			return fmt.Errorf("routing policy %s: at least one route step required", spec.ID)
		}
		for n, step := range spec.Route.Steps {
			if (step.Channel == "") == (step.ChannelGroup == "") {
				return fmt.Errorf("routing policy %s step %d: exactly one channel or channel_group required", spec.ID, n+1)
			}
			seenFailure := map[FailureType]bool{}
			for _, failure := range step.RecoverOn {
				if !FallbackAllowed(failure) {
					return fmt.Errorf("routing policy %s step %d: fallback on %q is not allowed", spec.ID, n+1, failure)
				}
				if seenFailure[failure] {
					return fmt.Errorf("routing policy %s step %d: recover_on values must be unique", spec.ID, n+1)
				}
				seenFailure[failure] = true
			}
		}
		if !spec.Enabled {
			continue
		}
		enabledSeen++
		if catchAllSeen {
			return fmt.Errorf("routing policy %s: enabled rule follows the catch-all", spec.ID)
		}
		if spec.Match.IsCatchAll() {
			catchAllSeen = true
		}
	}
	if len(specs) > 0 && enabledSeen == 0 {
		return fmt.Errorf("ordered routing policies require at least one enabled catch-all")
	}
	if enabledSeen > 0 && !catchAllSeen {
		return fmt.Errorf("enabled routing policies require a final catch-all")
	}
	return nil
}

func validateRoutingPolicyMatch(policyID string, match RoutingPolicyMatch) error {
	if err := validateRoutingPolicyStrings(policyID, "consumer", match.Consumers, false); err != nil {
		return err
	}
	if err := validateRoutingPolicyStrings(policyID, "model", match.Models, false); err != nil {
		return err
	}

	seenProtocols := map[Protocol]bool{}
	for _, protocol := range match.Protocols {
		if !ValidProtocol(protocol) {
			return fmt.Errorf("routing policy %s: unsupported protocol %q", policyID, protocol)
		}
		if seenProtocols[protocol] {
			return fmt.Errorf("routing policy %s: protocols must be unique", policyID)
		}
		seenProtocols[protocol] = true
	}

	if err := validateRoutingPolicyStrings(policyID, "required capability", match.RequiredCapabilities, true); err != nil {
		return err
	}
	for _, capability := range match.RequiredCapabilities {
		if _, known := NormalizeBooleanCapabilityName(capability); !known {
			return fmt.Errorf("routing policy %s: unknown required capability %q", policyID, capability)
		}
	}

	if err := validateRoutingPolicyStrings(policyID, "weekday", match.Weekdays, true); err != nil {
		return err
	}
	for _, weekday := range match.Weekdays {
		switch weekday {
		case "mon", "tue", "wed", "thu", "fri", "sat", "sun":
		default:
			return fmt.Errorf("routing policy %s: invalid weekday %q", policyID, weekday)
		}
	}

	if match.Timezone != "" {
		if strings.TrimSpace(match.Timezone) != match.Timezone {
			return fmt.Errorf("routing policy %s: timezone must be trimmed", policyID)
		}
		if _, err := time.LoadLocation(match.Timezone); err != nil {
			return fmt.Errorf("routing policy %s: invalid timezone %q", policyID, match.Timezone)
		}
	}
	if match.TimeWindow != nil &&
		(!validRoutingPolicyHHMM(match.TimeWindow.Start) || !validRoutingPolicyHHMM(match.TimeWindow.End)) {
		return fmt.Errorf("routing policy %s: invalid time window %q-%q", policyID, match.TimeWindow.Start, match.TimeWindow.End)
	}
	if strings.TrimSpace(match.Expression) != "" {
		return fmt.Errorf("routing policy %s: expression is not supported by this runtime", policyID)
	}
	return nil
}

func validateRoutingPolicyStrings(policyID, field string, values []string, lowercase bool) error {
	seen := map[string]bool{}
	for _, value := range values {
		if value == "" || strings.TrimSpace(value) != value || (lowercase && strings.ToLower(value) != value) {
			return fmt.Errorf("routing policy %s: %s values must be non-empty and canonical", policyID, field)
		}
		if seen[value] {
			return fmt.Errorf("routing policy %s: %s values must be unique", policyID, field)
		}
		seen[value] = true
	}
	return nil
}

func validRoutingPolicyHHMM(value string) bool {
	if len(value) != 5 || value[2] != ':' {
		return false
	}
	_, err := time.Parse("15:04", value)
	return err == nil
}

func FallbackAllowed(failure FailureType) bool {
	switch failure {
	case FailConnect, FailRateLimit, FailProvider5xx, FailQuotaExhausted,
		FailTTFTTimeout, FailStreamBroken, FailProtocolConversion, FailBadResponse:
		return true
	default:
		return false
	}
}
