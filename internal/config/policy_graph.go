package config

import (
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"ferry/internal/domain"
)

// PolicyGraphSpec is the lossless Low-Code source stored with each SQLite
// config version. Node coordinates are presentation only; edges and ports are
// the execution semantics.
type PolicyGraphSpec struct {
	Version int               `yaml:"version"`
	Nodes   []PolicyGraphNode `yaml:"nodes"`
	Edges   []PolicyGraphEdge `yaml:"edges"`
}

type PolicyGraphNode struct {
	ID   string         `yaml:"id"`
	Type string         `yaml:"type"`
	Data map[string]any `yaml:"data,omitempty"`
	X    float64        `yaml:"x,omitempty"`
	Y    float64        `yaml:"y,omitempty"`
}

type PolicyGraphEdge struct {
	ID             string   `yaml:"id"`
	Source         string   `yaml:"source"`
	SourcePort     string   `yaml:"source_port"`
	Target         string   `yaml:"target"`
	TargetPort     string   `yaml:"target_port,omitempty"`
	Kind           string   `yaml:"kind,omitempty"`
	RecoveryAction string   `yaml:"recovery_action,omitempty"`
	FailureTypes   []string `yaml:"failure_types,omitempty"`
	Priority       int      `yaml:"priority,omitempty"`
}

const (
	graphRecoverySameTier = "same_tier"
	graphRecoveryNextTier = "next_tier"
)

var graphPorts = map[string]map[string]bool{
	"input":     {"next": true},
	"schedule":  {"match": true, "otherwise": true},
	"condition": {"true": true, "false": true},
	"channel":   {"success": true, "recoverable_failure": true, "terminal_failure": true},
	"end":       {},
}

var requiredGraphPorts = map[string][]string{
	"input": {"next"}, "schedule": {"match", "otherwise"}, "condition": {"true", "false"}, "channel": {"success"},
}

var knownPolicyFailures = map[domain.FailureType]bool{
	domain.FailConnect: true, domain.FailAuth: true, domain.FailRateLimit: true,
	domain.FailProvider5xx: true, domain.FailQuotaExhausted: true,
	domain.FailTTFTTimeout: true, domain.FailUnsupportedParam: true,
	domain.FailContextOverflow: true, domain.FailStreamBroken: true,
	domain.FailProtocolConversion: true, domain.FailBadResponse: true, domain.FailClientCancel: true,
}

func (f *File) validatePolicyGraphs() error {
	names := make([]string, 0, len(f.PolicyGraphs))
	for name := range f.PolicyGraphs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		graph := f.PolicyGraphs[name]
		if err := validatePolicyGraph(name, graph); err != nil {
			return err
		}
	}
	return nil
}

// canonicalizePolicyGraphs makes the Low-Code graph the sole source of truth
// whenever one is present. The legacy Policies field remains the runtime
// compiler input, but its matching entry is deterministically regenerated on
// every validation. A client therefore cannot submit canvas A with policy B.
func (f *File) canonicalizePolicyGraphs() error {
	if len(f.PolicyGraphs) == 0 {
		return nil
	}
	if f.Policies == nil {
		f.Policies = map[string]PolicyYAML{}
	}
	names := make([]string, 0, len(f.PolicyGraphs))
	for name := range f.PolicyGraphs {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		compiled, err := compilePolicyGraph(name, f.PolicyGraphs[name], f.Server.Timezone)
		if err != nil {
			return err
		}
		f.Policies[name] = compiled
	}
	return nil
}

func validatePolicyGraph(name string, graph PolicyGraphSpec) error {
	if graph.Version != 1 {
		return fmt.Errorf("policy graph %s [GRAPH_VERSION_UNSUPPORTED]: version %d", name, graph.Version)
	}
	nodes := make(map[string]PolicyGraphNode, len(graph.Nodes))
	inputs := 0
	inputID := ""
	explicitTimezone := ""
	for _, node := range graph.Nodes {
		if node.ID == "" || nodes[node.ID].ID != "" {
			return fmt.Errorf("policy graph %s [DUPLICATE_NODE]: invalid or duplicate node %q", name, node.ID)
		}
		if _, ok := graphPorts[node.Type]; !ok {
			return fmt.Errorf("policy graph %s [UNKNOWN_NODE_TYPE]: node %s has type %q", name, node.ID, node.Type)
		}
		if node.Type == "input" {
			inputs++
			inputID = node.ID
		}
		if node.Type == "schedule" {
			if !validGraphHHMM(stringData(node.Data, "start")) || !validGraphHHMM(stringData(node.Data, "end")) {
				return fmt.Errorf("policy graph %s [INVALID_TIME_WINDOW]: node %s requires HH:MM start and end", name, node.ID)
			}
			if zone := stringData(node.Data, "timezone"); zone != "" {
				if _, err := time.LoadLocation(zone); err != nil {
					return fmt.Errorf("policy graph %s [INVALID_TIMEZONE]: node %s: %w", name, node.ID, err)
				}
				if explicitTimezone != "" && explicitTimezone != zone {
					return fmt.Errorf("policy graph %s [TIMEZONE_CONFLICT]: schedule nodes must share one runtime timezone", name)
				}
				explicitTimezone = zone
			}
		}
		if node.Type == "condition" {
			if err := ValidatePolicyExpression(stringData(node.Data, "expression")); err != nil {
				return fmt.Errorf("policy graph %s node %s: %w", name, node.ID, err)
			}
		}
		if node.Type == "end" {
			outcome := stringData(node.Data, "outcome")
			if outcome != "success" && outcome != "failure" {
				return fmt.Errorf("policy graph %s [END_OUTCOME_INVALID]: node %s must declare success or failure", name, node.ID)
			}
		}
		nodes[node.ID] = node
	}
	if inputs != 1 {
		return fmt.Errorf("policy graph %s [INPUT_COUNT]: exactly one start node is required", name)
	}

	connected := map[string]map[string]bool{}
	adj := map[string][]string{}
	outByPort := map[string]map[string]string{}
	incomingOrdinary := map[string]int{}
	edgeIDs := map[string]bool{}
	failures := map[string]map[string]bool{}
	recoveryRoutes := map[string][]PolicyGraphEdge{}
	for _, edge := range graph.Edges {
		if edge.ID == "" || edgeIDs[edge.ID] {
			return fmt.Errorf("policy graph %s [DUPLICATE_EDGE]: invalid or duplicate edge %q", name, edge.ID)
		}
		edgeIDs[edge.ID] = true
		source, sourceOK := nodes[edge.Source]
		target, targetOK := nodes[edge.Target]
		if !sourceOK || !targetOK {
			return fmt.Errorf("policy graph %s [DANGLING_EDGE]: edge %s references a missing node", name, edge.ID)
		}
		if !graphPorts[source.Type][edge.SourcePort] {
			return fmt.Errorf("policy graph %s [PORT_INCOMPATIBLE]: node %s has no %q output", name, edge.Source, edge.SourcePort)
		}
		if target.Type == "input" || (edge.TargetPort != "" && edge.TargetPort != "in") {
			return fmt.Errorf("policy graph %s [TARGET_PORT_INVALID]: edge %s must target a non-input node's in port", name, edge.ID)
		}
		failureEdge := edge.SourcePort == "recoverable_failure" || edge.SourcePort == "terminal_failure"
		if !failureEdge && len(edge.FailureTypes) > 0 {
			return fmt.Errorf("policy graph %s [FAILURE_TYPES_FORBIDDEN]: edge %s is not a failure edge", name, edge.ID)
		}
		if edge.SourcePort == "recoverable_failure" && target.Type != "channel" {
			return fmt.Errorf("policy graph %s [RECOVERABLE_TARGET_INVALID]: edge %s must target a channel", name, edge.ID)
		}
		if edge.SourcePort == "recoverable_failure" {
			switch edge.RecoveryAction {
			case graphRecoverySameTier:
				if edge.Source != edge.Target {
					return fmt.Errorf("policy graph %s [SAME_TIER_TARGET_INVALID]: edge %s must be a channel self-loop", name, edge.ID)
				}
			case graphRecoveryNextTier:
				// The full adjacent-channel chain is checked after topology order
				// has been derived.
			case "":
				return fmt.Errorf("policy graph %s [RECOVERY_ACTION_REQUIRED]: edge %s must declare same_tier or next_tier", name, edge.ID)
			default:
				return fmt.Errorf("policy graph %s [RECOVERY_ACTION_INVALID]: edge %s uses %q", name, edge.ID, edge.RecoveryAction)
			}
		} else if edge.RecoveryAction != "" {
			return fmt.Errorf("policy graph %s [RECOVERY_ACTION_FORBIDDEN]: edge %s is not recoverable", name, edge.ID)
		}
		if edge.SourcePort == "success" && (target.Type != "end" || stringData(target.Data, "outcome") != "success") {
			return fmt.Errorf("policy graph %s [SUCCESS_TARGET_INVALID]: edge %s must target a success end", name, edge.ID)
		}
		if edge.SourcePort == "terminal_failure" && (target.Type != "end" || stringData(target.Data, "outcome") != "failure") {
			return fmt.Errorf("policy graph %s [TERMINAL_TARGET_INVALID]: edge %s must target a failure end", name, edge.ID)
		}
		if source.Type == "input" && target.Type != "schedule" && target.Type != "condition" && target.Type != "channel" {
			return fmt.Errorf("policy graph %s [ENTRY_TARGET_INVALID]: start must lead to a decision or channel", name)
		}
		if (source.Type == "schedule" || source.Type == "condition") &&
			target.Type != "schedule" && target.Type != "condition" && target.Type != "channel" {
			return fmt.Errorf("policy graph %s [DECISION_TARGET_INVALID]: %s branch must reach a decision or channel", name, edge.Source)
		}
		if connected[edge.Source] == nil {
			connected[edge.Source] = map[string]bool{}
		}
		if connected[edge.Source][edge.SourcePort] && !failureEdge {
			return fmt.Errorf("policy graph %s [OUTPUT_PORT_FANOUT]: node %s port %s can have only one edge", name, edge.Source, edge.SourcePort)
		}
		connected[edge.Source][edge.SourcePort] = true
		if outByPort[edge.Source] == nil {
			outByPort[edge.Source] = map[string]string{}
		}
		if !failureEdge {
			outByPort[edge.Source][edge.SourcePort] = edge.Target
			incomingOrdinary[edge.Target]++
		}
		// A same-tier self-loop means "try the next eligible target in this
		// channel". It is runtime recovery metadata, not control-flow, so it
		// must not manufacture a graph cycle or affect reachability.
		if !isSameTierRecoverySelfLoop(edge) {
			adj[edge.Source] = append(adj[edge.Source], edge.Target)
		}
		if failureEdge {
			if len(edge.FailureTypes) == 0 {
				return fmt.Errorf("policy graph %s [FAILURE_TYPE_REQUIRED]: edge %s must select at least one failure type", name, edge.ID)
			}
			for _, failure := range edge.FailureTypes {
				failureType := domain.FailureType(failure)
				if !knownPolicyFailures[failureType] {
					return fmt.Errorf("policy graph %s [UNKNOWN_FAILURE_TYPE]: edge %s uses %q", name, edge.ID, failure)
				}
				key := edge.Source + "/" + failure
				if failures[key] == nil {
					failures[key] = map[string]bool{}
				}
				if failures[key][failure] {
					return fmt.Errorf("policy graph %s [FAILURE_TYPE_OVERLAP]: %s is routed twice from %s", name, failure, edge.Source)
				}
				failures[key][failure] = true
				recoveryRoutes[failure] = append(recoveryRoutes[failure], edge)
			}
		}
	}
	channelOrder := orderedPolicyGraphChannels(graph, nodes)
	if err := validatePolicyGraphRecoveryRoutes(name, channelOrder, recoveryRoutes); err != nil {
		return err
	}
	for _, node := range graph.Nodes {
		for _, port := range requiredGraphPorts[node.Type] {
			if !connected[node.ID][port] {
				return fmt.Errorf("policy graph %s [REQUIRED_PORT_UNCONNECTED]: node %s port %s", name, node.ID, port)
			}
		}
	}
	state := map[string]uint8{}
	var visit func(string) bool
	visit = func(id string) bool {
		if state[id] == 1 {
			return true
		}
		if state[id] == 2 {
			return false
		}
		state[id] = 1
		for _, next := range adj[id] {
			if visit(next) {
				return true
			}
		}
		state[id] = 2
		return false
	}
	for id := range nodes {
		if visit(id) {
			return fmt.Errorf("policy graph %s [GRAPH_CYCLE]: graph must be acyclic", name)
		}
	}
	reachable := map[string]bool{}
	var markReachable func(string)
	markReachable = func(id string) {
		if reachable[id] {
			return
		}
		reachable[id] = true
		for _, next := range adj[id] {
			markReachable(next)
		}
	}
	markReachable(inputID)
	for _, node := range graph.Nodes {
		if !reachable[node.ID] {
			return fmt.Errorf("policy graph %s [UNREACHABLE_NODE]: node %s (%s) is not reachable from start", name, node.ID, node.Type)
		}
		if (node.Type == "schedule" || node.Type == "condition") && incomingOrdinary[node.ID] > 1 {
			return fmt.Errorf("policy graph %s [DECISION_MERGE_UNSUPPORTED]: decision %s has multiple incoming branches", name, node.ID)
		}
	}
	for _, node := range graph.Nodes {
		if node.Type != "schedule" && node.Type != "condition" {
			continue
		}
		truePort, falsePort := "true", "false"
		if node.Type == "schedule" {
			truePort, falsePort = "match", "otherwise"
		}
		trueChannels := reachablePolicyGraphChannels(outByPort[node.ID][truePort], nodes, adj)
		falseChannels := reachablePolicyGraphChannels(outByPort[node.ID][falsePort], nodes, adj)
		exclusive := 0
		for channelID := range falseChannels {
			if !trueChannels[channelID] {
				exclusive++
			}
		}
		if exclusive == 0 {
			return fmt.Errorf("policy graph %s [DECISION_BRANCH_NOT_DISTINCT]: decision %s needs a channel exclusive to its false/otherwise branch", name, node.ID)
		}
	}
	return nil
}

func validatePolicyGraphRecoveryRoutes(name string, channelOrder []string, recoveryRoutes map[string][]PolicyGraphEdge) error {
	failures := make([]string, 0, len(recoveryRoutes))
	for failure := range recoveryRoutes {
		failures = append(failures, failure)
	}
	sort.Strings(failures)
	for _, failure := range failures {
		routes := recoveryRoutes[failure]
		action := ""
		bySource := make(map[string]PolicyGraphEdge, len(routes))
		for _, route := range routes {
			routeAction := route.RecoveryAction
			if route.SourcePort == "terminal_failure" {
				routeAction = "abort"
			}
			if action == "" {
				action = routeAction
			} else if action != routeAction {
				return fmt.Errorf("policy graph %s [AMBIGUOUS_RECOVERY]: %s mixes %s and %s routes", name, failure, action, routeAction)
			}
			bySource[route.Source] = route
		}

		switch action {
		case graphRecoverySameTier, "abort":
			for _, channelID := range channelOrder {
				if _, ok := bySource[channelID]; !ok {
					return fmt.Errorf("policy graph %s [RECOVERY_COVERAGE_INCOMPLETE]: %s %s route is missing for channel %s", name, failure, action, channelID)
				}
			}
			if len(bySource) != len(channelOrder) {
				return fmt.Errorf("policy graph %s [AMBIGUOUS_RECOVERY]: %s %s routes do not match the channel set", name, failure, action)
			}
		case graphRecoveryNextTier:
			for i := 0; i+1 < len(channelOrder); i++ {
				route, ok := bySource[channelOrder[i]]
				if !ok {
					return fmt.Errorf("policy graph %s [RECOVERY_CHAIN_INCOMPLETE]: %s lacks %s -> %s fallback", name, failure, channelOrder[i], channelOrder[i+1])
				}
				if route.Target != channelOrder[i+1] {
					return fmt.Errorf("policy graph %s [AMBIGUOUS_RECOVERY]: %s must target the immediately following channel", name, failure)
				}
			}
			if len(channelOrder) > 0 {
				if _, ok := bySource[channelOrder[len(channelOrder)-1]]; ok {
					return fmt.Errorf("policy graph %s [AMBIGUOUS_RECOVERY]: last channel cannot have a next-tier route for %s", name, failure)
				}
			}
			if len(bySource) != maxInt(len(channelOrder)-1, 0) {
				return fmt.Errorf("policy graph %s [AMBIGUOUS_RECOVERY]: %s next-tier routes do not form one adjacent chain", name, failure)
			}
		default:
			return fmt.Errorf("policy graph %s [AMBIGUOUS_RECOVERY]: %s has no representable recovery action", name, failure)
		}
	}
	return nil
}

func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}

func isSameTierRecoverySelfLoop(edge PolicyGraphEdge) bool {
	return edge.SourcePort == "recoverable_failure" &&
		edge.RecoveryAction == graphRecoverySameTier && edge.Source == edge.Target
}

func reachablePolicyGraphChannels(start string, nodes map[string]PolicyGraphNode, adj map[string][]string) map[string]bool {
	channels := map[string]bool{}
	visited := map[string]bool{}
	var visit func(string)
	visit = func(id string) {
		if id == "" || visited[id] {
			return
		}
		visited[id] = true
		if nodes[id].Type == "channel" {
			channels[id] = true
		}
		for _, next := range adj[id] {
			visit(next)
		}
	}
	visit(start)
	return channels
}

func orderedPolicyGraphChannels(graph PolicyGraphSpec, nodes map[string]PolicyGraphNode) []string {
	ordered := orderedPolicyGraphNodeIDs(graph, nodes)
	channels := make([]string, 0, len(ordered))
	for _, id := range ordered {
		if nodes[id].Type == "channel" {
			channels = append(channels, id)
		}
	}
	// Keep disconnected channels visible to the structural recovery audit so
	// older error codes remain actionable; the reachability gate rejects them
	// before any graph is compiled.
	seen := map[string]bool{}
	for _, id := range channels {
		seen[id] = true
	}
	for _, node := range graph.Nodes {
		if node.Type == "channel" && !seen[node.ID] {
			channels = append(channels, node.ID)
		}
	}
	return channels
}

func orderedPolicyGraphNodeIDs(graph PolicyGraphSpec, nodes map[string]PolicyGraphNode) []string {
	type indexedEdge struct {
		PolicyGraphEdge
		index int
	}
	portRank := map[string]int{
		"next": 0, "otherwise": 0, "false": 0, "recoverable_failure": 0,
		"match": 1, "true": 1, "success": 2, "terminal_failure": 3,
	}
	outgoing := map[string][]indexedEdge{}
	for i, edge := range graph.Edges {
		if isSameTierRecoverySelfLoop(edge) {
			continue
		}
		outgoing[edge.Source] = append(outgoing[edge.Source], indexedEdge{PolicyGraphEdge: edge, index: i})
	}
	for source := range outgoing {
		sort.SliceStable(outgoing[source], func(i, j int) bool {
			a, b := outgoing[source][i], outgoing[source][j]
			ra, oka := portRank[a.SourcePort]
			rb, okb := portRank[b.SourcePort]
			if !oka {
				ra = 9
			}
			if !okb {
				rb = 9
			}
			if ra != rb {
				return ra < rb
			}
			if a.Priority != b.Priority {
				return a.Priority < b.Priority
			}
			if a.ID != b.ID {
				return a.ID < b.ID
			}
			if a.Target != b.Target {
				return a.Target < b.Target
			}
			return a.index < b.index
		})
	}
	visited := map[string]bool{}
	order := []string{}
	var visit func(string)
	visit = func(id string) {
		if id == "" || visited[id] {
			return
		}
		visited[id] = true
		_, ok := nodes[id]
		if !ok {
			return
		}
		order = append(order, id)
		for _, edge := range outgoing[id] {
			visit(edge.Target)
		}
	}
	for _, node := range graph.Nodes {
		if node.Type == "input" {
			visit(node.ID)
			break
		}
	}
	return order
}

func compilePolicyGraph(name string, graph PolicyGraphSpec, defaultTimezone string) (PolicyYAML, error) {
	if err := validatePolicyGraph(name, graph); err != nil {
		return PolicyYAML{}, err
	}
	nodes := make(map[string]PolicyGraphNode, len(graph.Nodes))
	adj := make(map[string][]string, len(graph.Nodes))
	outByPort := make(map[string]map[string]string, len(graph.Nodes))
	for _, node := range graph.Nodes {
		nodes[node.ID] = node
	}
	for _, edge := range graph.Edges {
		if !isSameTierRecoverySelfLoop(edge) {
			adj[edge.Source] = append(adj[edge.Source], edge.Target)
		}
		if outByPort[edge.Source] == nil {
			outByPort[edge.Source] = map[string]string{}
		}
		if edge.SourcePort != "recoverable_failure" && edge.SourcePort != "terminal_failure" {
			outByPort[edge.Source][edge.SourcePort] = edge.Target
		}
	}
	orderedIDs := orderedPolicyGraphNodeIDs(graph, nodes)
	channelIDs := make([]string, 0)
	for _, id := range orderedIDs {
		if nodes[id].Type == "channel" {
			channelIDs = append(channelIDs, id)
		}
	}

	policy := PolicyYAML{Recovery: map[string]domain.RecoveryAction{}}
	// The runtime Policy has one timezone. All explicit schedule zones were
	// proven equal by validation, so that unique value wins regardless of node
	// storage/traversal order; otherwise schedules inherit the server default.
	for _, id := range orderedIDs {
		node := nodes[id]
		if node.Type != "schedule" {
			continue
		}
		zone := stringData(node.Data, "timezone")
		if zone != "" {
			policy.Timezone = zone
			break
		}
	}
	if policy.Timezone == "" {
		policy.Timezone = defaultTimezone
	}
	if policy.Timezone == "" {
		policy.Timezone = "Asia/Shanghai"
	}

	channelIndex := map[string]int{}
	channelPool := map[string]string{}
	channelTierName := map[string]string{}
	for index, id := range channelIDs {
		node := nodes[id]
		pool := firstStringData(node.Data, "internalPool", "internal_pool", "channelID", "channel_id", "pool")
		if pool == "" {
			return PolicyYAML{}, fmt.Errorf("policy graph %s [CHANNEL_POOL_REQUIRED]: channel %s must select a pool", name, id)
		}
		tierName := firstStringData(node.Data, "tierName", "tier_name", "label")
		if tierName == "" {
			tierName = fmt.Sprintf("候选层 %d", index+1)
		}
		channelIndex[id] = index
		channelPool[id] = pool
		channelTierName[id] = tierName
		policy.Tiers = append(policy.Tiers, domain.TierDef{
			Name:      tierName,
			Pool:      pool,
			Objective: stringData(node.Data, "objective"),
		})
	}

	for _, id := range orderedIDs {
		node := nodes[id]
		if node.Type != "schedule" && node.Type != "condition" {
			continue
		}
		truePort, falsePort := "true", "false"
		if node.Type == "schedule" {
			truePort, falsePort = "match", "otherwise"
		}
		trueChannels := reachablePolicyGraphChannels(outByPort[id][truePort], nodes, adj)
		falseChannels := reachablePolicyGraphChannels(outByPort[id][falsePort], nodes, adj)
		exclusive := make([]string, 0)
		for channelID := range falseChannels {
			if !trueChannels[channelID] {
				exclusive = append(exclusive, channelID)
			}
		}
		sort.Slice(exclusive, func(i, j int) bool { return channelIndex[exclusive[i]] < channelIndex[exclusive[j]] })

		var when domain.RuleWhen
		var err error
		if node.Type == "schedule" {
			when.TimeWindow = &domain.TimeWindow{Start: stringData(node.Data, "start"), End: stringData(node.Data, "end")}
		} else {
			when, err = compilePolicyExpression(stringData(node.Data, "expression"))
			if err != nil {
				return PolicyYAML{}, fmt.Errorf("policy graph %s node %s: %w", name, id, err)
			}
		}
		label := stringData(node.Data, "label")
		if label == "" {
			label = "条件 " + id
		}
		for _, channelID := range exclusive {
			ruleWhen := when
			ruleWhen.Pool = channelPool[channelID]
			exclude := true
			policy.Rules = append(policy.Rules, domain.PolicyRule{
				Name:   fmt.Sprintf("%s · 排除 %s", label, channelTierName[channelID]),
				When:   ruleWhen,
				Action: domain.RuleAction{Exclude: &exclude},
			})
		}
	}

	for failure := range knownPolicyFailures {
		policy.Recovery[string(failure)] = domain.RecoveryAction{Abort: true}
	}
	for _, edge := range graph.Edges {
		for _, failure := range edge.FailureTypes {
			switch edge.SourcePort {
			case "recoverable_failure":
				switch edge.RecoveryAction {
				case graphRecoverySameTier:
					policy.Recovery[failure] = domain.RecoveryAction{NextTarget: "same_tier"}
				case graphRecoveryNextTier:
					policy.Recovery[failure] = domain.RecoveryAction{NextTier: true}
				}
			case "terminal_failure":
				policy.Recovery[failure] = domain.RecoveryAction{Abort: true}
			}
		}
	}
	return policy, nil
}

func firstStringData(data map[string]any, keys ...string) string {
	for _, key := range keys {
		if value := stringData(data, key); value != "" {
			return value
		}
	}
	return ""
}

var graphHHMM = regexp.MustCompile(`^(?:[01][0-9]|2[0-3]):[0-5][0-9]$`)

func validGraphHHMM(value string) bool { return graphHHMM.MatchString(value) }

func stringData(data map[string]any, key string) string {
	if data == nil {
		return ""
	}
	v, _ := data[key].(string)
	return v
}

var expressionChars = regexp.MustCompile(`^[A-Za-z0-9_\.\s"'()<>=!&|,\[\]-]+$`)
var expressionStrings = regexp.MustCompile(`"(?:[^"\\]|\\.)*"|'(?:[^'\\]|\\.)*'`)
var expressionIdentifiers = regexp.MustCompile(`[A-Za-z_][A-Za-z0-9_.]*`)

var allowedExpressionIdentifiers = map[string]bool{
	"true": true, "false": true, "null": true, "in": true,
	"context.local_hour": true,
	"context.weekday":    true,
	"request.alias":      true, "request.model": true, "request.stream": true, "request.max_tokens": true,
	"consumer.name": true,
	"channel.id":    true, "channel.health": true, "channel.cooldown": true,
	"channel.quota_remaining": true, "channel.quota_confidence": true, "channel.quota_multiplier": true,
}

var (
	expressionLocalHours      = regexp.MustCompile(`^\s*context\.local_hour\s*>=\s*(\d{1,2})\s*&&\s*context\.local_hour\s*<\s*(\d{1,2})\s*$`)
	expressionConsumer        = regexp.MustCompile(`^\s*consumer\.name\s*==\s*["'][^"']+["']\s*$`)
	expressionHealth          = regexp.MustCompile(`^\s*channel\.health\s*==\s*["'](?:healthy|unhealthy)["']\s*$`)
	expressionQuota           = regexp.MustCompile(`^\s*channel\.quota_remaining\s*(?:>=|<=|>|<|==)\s*(\d+(?:\.\d+)?)\s*$`)
	expressionMultiplier      = regexp.MustCompile(`^\s*channel\.quota_multiplier\s*(?:>=|<=|>|<|==)\s*(\d+(?:\.\d+)?)\s*$`)
	expressionConfidence      = regexp.MustCompile(`^\s*channel\.quota_confidence\s*==\s*["'](?:high|medium|low)["']\s*$`)
	expressionCooldown        = regexp.MustCompile(`^\s*channel\.cooldown\s*==\s*true\s*$`)
	expressionWeekdays        = regexp.MustCompile(`^\s*context\.weekday\s+in\s+\[\s*["'](?:mon|tue|wed|thu|fri|sat|sun)["'](?:\s*,\s*["'](?:mon|tue|wed|thu|fri|sat|sun)["'])*\s*\]\s*$`)
	expressionConsumerParts   = regexp.MustCompile(`^\s*consumer\.name\s*==\s*["']([^"']+)["']\s*$`)
	expressionHealthParts     = regexp.MustCompile(`^\s*channel\.health\s*==\s*["'](healthy|unhealthy)["']\s*$`)
	expressionQuotaParts      = regexp.MustCompile(`^\s*channel\.quota_remaining\s*(>=|<=|>|<|==)\s*(\d+(?:\.\d+)?)\s*$`)
	expressionMultiplierParts = regexp.MustCompile(`^\s*channel\.quota_multiplier\s*(>=|<=|>|<|==)\s*(\d+(?:\.\d+)?)\s*$`)
	expressionConfidenceParts = regexp.MustCompile(`^\s*channel\.quota_confidence\s*==\s*["'](high|medium|low)["']\s*$`)
	expressionWeekdayValue    = regexp.MustCompile(`["'](mon|tue|wed|thu|fri|sat|sun)["']`)
)

func supportedPolicyExpression(expr string) bool {
	if match := expressionLocalHours.FindStringSubmatch(expr); match != nil {
		start, _ := strconv.Atoi(match[1])
		end, _ := strconv.Atoi(match[2])
		return start >= 0 && start <= 23 && end >= 1 && end <= 23 && start < end
	}
	if expressionConsumer.MatchString(expr) || expressionHealth.MatchString(expr) ||
		expressionConfidence.MatchString(expr) || expressionCooldown.MatchString(expr) ||
		expressionWeekdays.MatchString(expr) {
		return true
	}
	if match := expressionQuota.FindStringSubmatch(expr); match != nil {
		value, _ := strconv.ParseFloat(match[1], 64)
		return value >= 0 && value <= 1
	}
	if match := expressionMultiplier.FindStringSubmatch(expr); match != nil {
		value, _ := strconv.ParseFloat(match[1], 64)
		return value >= 0
	}
	return false
}

func compilePolicyExpression(expr string) (domain.RuleWhen, error) {
	if err := ValidatePolicyExpression(expr); err != nil {
		return domain.RuleWhen{}, err
	}
	if match := expressionLocalHours.FindStringSubmatch(expr); match != nil {
		start, _ := strconv.Atoi(match[1])
		end, _ := strconv.Atoi(match[2])
		return domain.RuleWhen{TimeWindow: &domain.TimeWindow{
			Start: fmt.Sprintf("%02d:00", start), End: fmt.Sprintf("%02d:00", end),
		}}, nil
	}
	if match := expressionConsumerParts.FindStringSubmatch(expr); match != nil {
		return domain.RuleWhen{ConsumerKey: match[1]}, nil
	}
	if match := expressionHealthParts.FindStringSubmatch(expr); match != nil {
		return domain.RuleWhen{Health: match[1]}, nil
	}
	if match := expressionQuotaParts.FindStringSubmatch(expr); match != nil {
		value, _ := strconv.ParseFloat(match[2], 64)
		return domain.RuleWhen{QuotaRemaining: policyFloatRange(match[1], value)}, nil
	}
	if match := expressionMultiplierParts.FindStringSubmatch(expr); match != nil {
		value, _ := strconv.ParseFloat(match[2], 64)
		return domain.RuleWhen{QuotaMultiplier: policyFloatRange(match[1], value)}, nil
	}
	if match := expressionConfidenceParts.FindStringSubmatch(expr); match != nil {
		return domain.RuleWhen{QuotaConfidence: match[1]}, nil
	}
	if expressionCooldown.MatchString(expr) {
		return domain.RuleWhen{Cooldown: true}, nil
	}
	if expressionWeekdays.MatchString(expr) {
		matches := expressionWeekdayValue.FindAllStringSubmatch(expr, -1)
		weekdays := make([]string, 0, len(matches))
		for _, match := range matches {
			weekdays = append(weekdays, match[1])
		}
		return domain.RuleWhen{Weekdays: weekdays}, nil
	}
	return domain.RuleWhen{}, fmt.Errorf("[EXPRESSION_UNSUPPORTED]: expression cannot be compiled by the current Low-Code runtime")
}

func policyFloatRange(operator string, value float64) *domain.FloatRange {
	rangeValue := &domain.FloatRange{}
	switch operator {
	case ">=":
		rangeValue.Gte = &value
	case "<=":
		rangeValue.Lte = &value
	case ">":
		rangeValue.Gt = &value
	case "<":
		rangeValue.Lt = &value
	case "==":
		rangeValue.Eq = &value
	}
	return rangeValue
}

// ValidatePolicyExpression validates the deliberately small, pure CEL-like
// boolean subset used by the B-side condition node. It has no functions,
// assignment, object construction, network, filesystem, headers, or secrets.
func ValidatePolicyExpression(expr string) error {
	if expr == "" || len(expr) > 512 {
		return fmt.Errorf("[EXPRESSION_INVALID]: expression length must be 1..512")
	}
	lower := strings.ToLower(expr)
	for _, token := range []string{"fetch", "globalthis", "constructor", "function", "=>", "new ", ";", "{"} {
		if strings.Contains(lower, token) {
			return fmt.Errorf("[EXPRESSION_FORBIDDEN]: token %q is not allowed", token)
		}
	}
	if !expressionChars.MatchString(expr) {
		return fmt.Errorf("[EXPRESSION_FORBIDDEN]: expression contains unsupported characters")
	}
	withoutStrings := expressionStrings.ReplaceAllString(expr, "")
	for _, id := range expressionIdentifiers.FindAllString(withoutStrings, -1) {
		if !allowedExpressionIdentifiers[id] {
			return fmt.Errorf("[EXPRESSION_UNKNOWN_FIELD]: %s", id)
		}
	}
	if !strings.ContainsAny(expr, "<>=!") && !strings.Contains(expr, "&&") && !strings.Contains(expr, "||") && !strings.Contains(expr, " in ") {
		return fmt.Errorf("[EXPRESSION_NOT_BOOLEAN]: expression must produce bool")
	}
	if !supportedPolicyExpression(expr) {
		return fmt.Errorf("[EXPRESSION_UNSUPPORTED]: expression cannot be compiled by the current Low-Code runtime")
	}
	return nil
}
