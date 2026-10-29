package domain

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestOpenAIImagesIsValidAndNativeOnly(t *testing.T) {
	if !ValidProtocol(ProtocolOpenAIImages) {
		t.Fatal("openai-images must be a valid protocol")
	}
	imageTarget := &CompiledTarget{NativeProtocols: []Protocol{ProtocolOpenAIImages}, ProtocolConversion: map[Protocol]bool{ProtocolOpenAIChat: true}}
	if !imageTarget.AcceptsProtocol(ProtocolOpenAIImages) || imageTarget.AcceptsProtocol(ProtocolOpenAIChat) {
		t.Fatalf("image target protocol acceptance is not native-only")
	}
	textTarget := &CompiledTarget{NativeProtocols: []Protocol{ProtocolOpenAIChat}, ProtocolConversion: map[Protocol]bool{ProtocolOpenAIImages: true}}
	if textTarget.AcceptsProtocol(ProtocolOpenAIImages) {
		t.Fatal("text target must not accept image conversion")
	}
	mixedTarget := &CompiledTarget{NativeProtocols: []Protocol{ProtocolOpenAIChat, ProtocolOpenAIImages}, ProtocolConversion: map[Protocol]bool{ProtocolOpenAIResponses: true}}
	if !mixedTarget.AcceptsProtocol(ProtocolOpenAIResponses) || mixedTarget.EgressProtocol(ProtocolOpenAIResponses) != ProtocolOpenAIChat {
		t.Fatal("a mixed target must still convert between its text protocols")
	}
}

func TestTextProtocolConversionMatrixUsesNativeFirst(t *testing.T) {
	textProtocols := []Protocol{ProtocolOpenAIChat, ProtocolOpenAIResponses, ProtocolAnthropic}
	for _, native := range textProtocols {
		t.Run(string(native), func(t *testing.T) {
			conversion := map[Protocol]bool{ProtocolOpenAIImages: true}
			for _, ingress := range textProtocols {
				conversion[ingress] = true
			}
			target := &CompiledTarget{
				NativeProtocols:    []Protocol{native},
				ProtocolConversion: conversion,
			}
			for _, ingress := range textProtocols {
				if !target.AcceptsProtocol(ingress) {
					t.Fatalf("%s-native target rejected %s ingress", native, ingress)
				}
				if got := target.EgressProtocol(ingress); got != native {
					t.Fatalf("%s ingress egress=%s, want native %s", ingress, got, native)
				}
			}
			if target.AcceptsProtocol(ProtocolOpenAIImages) {
				t.Fatal("text target accepted image conversion despite an enabled flag")
			}

			for _, ingress := range textProtocols {
				target.ProtocolConversion[ingress] = false
				want := ingress == native
				if got := target.AcceptsProtocol(ingress); got != want {
					t.Fatalf("conversion-disabled %s ingress accepted=%t, want %t", ingress, got, want)
				}
			}
		})
	}
}

func TestCodexProviderUsesResponsesNativelyAndConvertsChatAndAnthropic(t *testing.T) {
	provider := &Provider{
		Type: ProviderCliproxyCodex,
		ProtocolConversion: map[Protocol]bool{
			ProtocolOpenAIChat:   false,
			ProtocolAnthropic:    false,
			ProtocolOpenAIImages: true,
		},
	}
	for _, protocol := range []Protocol{ProtocolOpenAIResponses, ProtocolOpenAIChat, ProtocolAnthropic} {
		if !provider.AllowsProtocolConversion(protocol) {
			t.Fatalf("Codex provider rejected supported ingress protocol %s", protocol)
		}
	}
	if provider.AllowsProtocolConversion(ProtocolOpenAIImages) {
		t.Fatal("Codex OAuth provider must not claim image conversion")
	}
}

// ---------------------------------------------------------------------------
// Failure taxonomy (PRD §6.8)

func TestRecoveryClass(t *testing.T) {
	cases := []struct {
		ft   FailureType
		want RecoveryClass
	}{
		{FailQuotaExhausted, RecoverNextTier},
		{FailContextOverflow, RecoverAbort},
		{FailClientCancel, RecoverAbort},
		{FailUnsupportedParam, RecoverAbort},
		{FailConnect, RecoverNextTargetSameTier},
		{FailAuth, RecoverAbort},
		{FailRateLimit, RecoverNextTargetSameTier},
		{FailProvider5xx, RecoverNextTargetSameTier},
		{FailTTFTTimeout, RecoverNextTargetSameTier},
		{FailStreamBroken, RecoverNextTargetSameTier},
		{FailProtocolConversion, RecoverNextTargetSameTier},
		{FailBadResponse, RecoverNextTargetSameTier},
	}
	for _, c := range cases {
		if got := c.ft.RecoveryClass(); got != c.want {
			t.Errorf("%s.RecoveryClass() = %q, want %q", c.ft, got, c.want)
		}
	}
}

func TestFallbackAllowedIncludesInvalidProviderResponse(t *testing.T) {
	if !FallbackAllowed(FailBadResponse) {
		t.Fatal("a target-scoped invalid provider response must be eligible for an explicitly configured fallback")
	}
	if FallbackAllowed(FailAuth) || FallbackAllowed(FailContextOverflow) {
		t.Fatal("request or credential failures must remain fail-closed")
	}
}

func TestCircuitAction(t *testing.T) {
	neutral := map[FailureType]bool{
		FailQuotaExhausted: true, FailClientCancel: true, FailContextOverflow: true,
		FailUnsupportedParam:   true,
		FailProtocolConversion: true,
	}
	all := []FailureType{FailConnect, FailAuth, FailRateLimit, FailProvider5xx,
		FailTTFTTimeout, FailUnsupportedParam, FailStreamBroken, FailProtocolConversion, FailBadResponse,
		FailQuotaExhausted, FailClientCancel, FailContextOverflow}
	for _, ft := range all {
		want := CircuitCountFailure
		if neutral[ft] {
			want = CircuitNeutral
		}
		if got := ft.CircuitAction(); got != want {
			t.Errorf("%s.CircuitAction() = %q, want %q", ft, got, want)
		}
	}
}

func TestFailureError(t *testing.T) {
	var nilF *Failure
	if got := nilF.Error(); got != "<nil>" {
		t.Errorf("nil Failure.Error() = %q, want <nil>", got)
	}
	f := &Failure{Type: FailRateLimit, HTTPStatus: 429, Message: "slow down"}
	if got, want := f.Error(), "rate_limit (http 429): slow down"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
	f2 := &Failure{Type: FailConnect, Message: "dial refused"}
	if got, want := f2.Error(), "connect_failure: dial refused"; got != want {
		t.Errorf("got %q, want %q", got, want)
	}
}

func TestFailureEffectiveScope(t *testing.T) {
	cases := []struct {
		failure *Failure
		want    FailureScope
	}{
		{&Failure{Type: FailContextOverflow}, FailureScopeRequest},
		{&Failure{Type: FailRateLimit}, FailureScopeConnection},
		{&Failure{Type: FailProvider5xx}, FailureScopeProvider},
		{&Failure{Type: FailStreamBroken}, FailureScopeStream},
		{&Failure{Type: FailProtocolConversion}, FailureScopeTarget},
		{&Failure{Type: FailBadResponse}, FailureScopeTarget},
		{&Failure{Type: FailRateLimit, Scope: FailureScopeProvider}, FailureScopeProvider},
	}
	for _, tc := range cases {
		if got := tc.failure.EffectiveScope(); got != tc.want {
			t.Errorf("%s scope = %s, want %s", tc.failure.Type, got, tc.want)
		}
	}
}

func TestClassifyHTTP(t *testing.T) {
	cases := []struct {
		status int
		body   string
		want   FailureType
	}{
		{401, "", FailAuth},
		{403, "forbidden", FailAuth},
		{404, "not found", FailBadResponse},
		{429, "rate limited", FailRateLimit},
		{429, `{"error":{"type":"usage_limit_reached"}}`, FailQuotaExhausted},
		{500, "oops", FailProvider5xx},
		{503, "maintenance", FailProvider5xx},
		{400, `{"error":"This model's maximum context length is 8192 tokens"}`, FailContextOverflow},
		{400, `"context_length_exceeded"`, FailContextOverflow},
		{400, `"prompt is too long"`, FailContextOverflow},
		{400, `"Insufficient balance"`, FailQuotaExhausted},
		{402, "quota exceeded for this month", FailQuotaExhausted},
		{400, "bad request shape", FailBadResponse},
		{400, "temperature only supports 1", FailUnsupportedParam},
		{400, "max_tokens must be 128", FailBadResponse},
		{0, "tool_choice is not supported when thinking is enabled", FailUnsupportedParam},
		{200, "", FailBadResponse},
	}
	for _, c := range cases {
		got := ClassifyHTTP(c.status, c.body)
		if got != c.want {
			t.Errorf("ClassifyHTTP(%d, %q) = %s, want %s", c.status, c.body, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// Capabilities / DegradationAllowed

func TestSatisfies(t *testing.T) {
	full := Capabilities{Text: true, Images: true, Stream: true, Tools: true, Reasoning: true,
		StructuredOutput: true, Vision: true, ParallelTools: true,
		ContextWindow: 128 * 1024, MaxOutput: 8192}
	req := Requirements{Text: true, Tools: true, MinContext: 32000, MinOutput: 4096}
	if !full.Satisfies(req) {
		t.Fatal("full caps should satisfy")
	}
	noCtx := full
	noCtx.ContextWindow = 0
	if !noCtx.Satisfies(req) {
		t.Error("unknown context window must not fail the min-context check")
	}
	small := full
	small.ContextWindow = 8000
	if small.Satisfies(req) {
		t.Error("8000 window must fail a 32000-token requirement")
	}
	smallOutput := full
	smallOutput.MaxOutput = 2048
	if smallOutput.Satisfies(req) {
		t.Error("2048 max output must fail a 4096-token output requirement")
	}
	unknownOutput := full
	unknownOutput.MaxOutput = 0
	if !unknownOutput.Satisfies(req) {
		t.Error("unknown maximum output must not claim a known incompatibility")
	}
	zeroReq := Requirements{Text: false}
	if !(Capabilities{}).Satisfies(zeroReq) {
		t.Error("empty requirements satisfied by anything")
	}
	noTools := full
	noTools.Tools = false
	if noTools.Satisfies(req) {
		t.Error("missing tools must fail")
	}
	if full.Satisfies(Requirements{Images: true}) == false ||
		(Capabilities{Text: true}).Satisfies(Requirements{Images: true}) {
		t.Error("Images must be an independent hard capability gate")
	}
	if got := NormalizeProtocolCapabilities(Capabilities{}, []Protocol{ProtocolOpenAIImages}); !got.Images || got.Text || got.Vision {
		t.Errorf("Images protocol normalization = %+v", got)
	}
}

func TestDegradationAllowed(t *testing.T) {
	cases := []struct {
		allow bool
		level CompatLevel
		want  bool
	}{
		{false, CompatExact, true},
		{false, CompatMapped, true},
		{false, CompatDegraded, true},
		{true, CompatDegraded, true},     // legacy flag does not override lenient default
		{true, CompatUnsupported, false}, // never allowed
		{false, CompatUnsupported, false},
	}
	for _, c := range cases {
		if got := DegradationAllowed(c.allow, c.level); got != c.want {
			t.Errorf("DegradationAllowed(%v,%s)=%v want %v", c.allow, c.level, got, c.want)
		}
	}
}

// ---------------------------------------------------------------------------
// EstimateContext / ExtractRequirements

func TestEstimateContext(t *testing.T) {
	rc := &RequestContract{
		Messages: []Message{{Role: RoleUser, Blocks: []Block{
			{Kind: BlockText, Text: strings.Repeat("a", 300)},
		}}},
	}
	// 300 chars / 3 = 100 tokens + margin 8192 (default)
	if got := rc.EstimateContext(); got != 8292 {
		t.Errorf("EstimateContext()=%d want 8292", got)
	}
	rc.MaxTokens = 900 // 100 + 900 = 1000 -> floored up to 1024
	if got := rc.EstimateContext(); got != 1024 {
		t.Errorf("with MaxTokens=900 got %d want 1024", got)
	}
	tiny := &RequestContract{}
	if got := tiny.EstimateContext(); got != 8192 { // 0 chars + default margin
		t.Errorf("empty contract: got %d want 8192", got)
	}
	img := &RequestContract{Messages: []Message{{Blocks: []Block{{Kind: BlockImage, ImageData: make([]byte, 600)}}}}}
	// images count as a flat +2000 chars (binary size ignored): 2000/3 + 8192
	want := int64(2000/3 + 8192)
	if got := img.EstimateContext(); got != want {
		t.Errorf("image: got %d want %d", got, want)
	}
}

func TestExtractRequirements(t *testing.T) {
	parallel := true
	rc := &RequestContract{
		Stream:         true,
		Tools:          []ToolDef{{Name: "f"}},
		ParallelTools:  &parallel,
		ResponseFormat: &ResponseFormat{},
		Reasoning:      &ReasoningSpec{Effort: "high"},
		MaxTokens:      3000,
		Messages: []Message{{Role: RoleUser, Blocks: []Block{
			{Kind: BlockText, Text: "hi"},
			{Kind: BlockImage, ImageMIME: "image/png"},
		}}},
	}
	req := rc.ExtractRequirements()
	if !req.Text || !req.Stream || !req.Tools || !req.ParallelTools || !req.StructuredOutput || !req.Reasoning || !req.Vision {
		t.Fatalf("requirements wrong: %+v", req)
	}
	if req.MinContext != rc.EstimateContext() {
		t.Errorf("MinContext should equal EstimateContext")
	}
	if req.MinOutput != rc.MaxTokens {
		t.Errorf("MinOutput=%d want %d", req.MinOutput, rc.MaxTokens)
	}

	// Tool/reasoning history is itself a protocol requirement even if the
	// continuation omits a fresh tool definition or reasoning control.
	history := (&RequestContract{Messages: []Message{
		{Role: RoleAssistant, Blocks: []Block{{Kind: BlockThinking, Text: "private"}, {Kind: BlockToolUse, ID: "call-1"}}},
		{Role: RoleTool, ToolID: "call-1", Blocks: []Block{{Kind: BlockToolResult, ID: "call-1"}}},
	}}).ExtractRequirements()
	if !history.Tools || !history.Reasoning {
		t.Fatalf("history requirements were dropped: %+v", history)
	}

	// Native-only content remains exact on its source protocol, but routing
	// must still enforce every capability implied by its known semantics.
	native := (&RequestContract{
		ToolChoice:         "required",
		NativeRequirements: Requirements{StructuredOutput: true},
		Messages: []Message{{Role: RoleUser, Blocks: []Block{
			{Kind: BlockOpaque, OpaqueType: "image_url"},
			{Kind: BlockOpaque, OpaqueType: "reasoning"},
			{Kind: BlockOpaque, OpaqueType: "computer_call"},
		}}},
	}).ExtractRequirements()
	if !native.Tools || !native.Reasoning || !native.StructuredOutput || !native.Vision {
		t.Fatalf("native requirements were dropped: %+v", native)
	}
	unknown := (&RequestContract{Messages: []Message{{Blocks: []Block{{
		Kind: BlockOpaque, OpaqueType: "input_audio",
	}}}}}).ExtractRequirements()
	if unknown.Tools || unknown.Reasoning || unknown.StructuredOutput || unknown.Vision {
		t.Fatalf("unknown opaque type was guessed as an existing capability: %+v", unknown)
	}

	image := (&RequestContract{
		Protocol: ProtocolOpenAIImages, MaxTokens: 4000,
		Messages: []Message{{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "draw a cat"}}}},
	}).ExtractRequirements()
	if !image.Images || image.Text || image.Vision || image.Stream || image.MinContext != 0 || image.MinOutput != 0 {
		t.Fatalf("native Images request was confused with text/vision: %+v", image)
	}
}

func TestValidateToolSequence(t *testing.T) {
	valid := &RequestContract{Messages: []Message{
		{Role: RoleAssistant, Blocks: []Block{
			{Kind: BlockToolUse, ID: "call-a", Name: "alpha", Input: json.RawMessage(`{"a":1}`)},
			{Kind: BlockToolUse, ID: "call-b", Name: "beta", Input: json.RawMessage(`{}`)},
		}},
		{Role: RoleTool, ToolID: "call-a", Blocks: []Block{{Kind: BlockText, Text: "one"}}},
		{Role: RoleTool, ToolID: "call-b", Blocks: []Block{{Kind: BlockText, Text: "two"}}},
		{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "continue"}}},
	}}
	if err := valid.ValidateToolSequence(); err != nil {
		t.Fatalf("valid parallel tool transcript: %v", err)
	}

	missing := &RequestContract{Messages: []Message{
		{Role: RoleAssistant, Blocks: []Block{{Kind: BlockToolUse, ID: "call-a", Name: "alpha", Input: json.RawMessage(`{}`)}}},
		{Role: RoleUser, Blocks: []Block{{Kind: BlockText, Text: "skip result"}}},
	}}
	if err := missing.ValidateToolSequence(); err == nil || !strings.Contains(err.Error(), "immediate tool results") {
		t.Fatalf("missing tool result was accepted: %v", err)
	}

	continuation := &RequestContract{Continuation: &NativeContinuation{NativeResponseID: "resp-1"},
		Messages: []Message{{Role: RoleTool, ToolID: "call-native", Blocks: []Block{{Kind: BlockText, Text: "ok"}}}}}
	if err := continuation.ValidateToolSequence(); err != nil {
		t.Fatalf("native continuation leading result: %v", err)
	}

	invalidJSON := &RequestContract{Messages: []Message{{Role: RoleAssistant,
		Blocks: []Block{{Kind: BlockToolUse, ID: "call-a", Name: "alpha", Input: json.RawMessage(`{"x":`)}}}}}
	if err := invalidJSON.ValidateToolSequence(); err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("invalid tool arguments were accepted: %v", err)
	}
	missingArguments := &RequestContract{Messages: []Message{{Role: RoleAssistant,
		Blocks: []Block{{Kind: BlockToolUse, ID: "call-a", Name: "alpha"}}}}}
	if err := missingArguments.ValidateToolSequence(); err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("missing tool arguments were accepted: %v", err)
	}
	arrayArguments := &RequestContract{Messages: []Message{{Role: RoleAssistant,
		Blocks: []Block{{Kind: BlockToolUse, ID: "call-a", Name: "alpha", Input: json.RawMessage(`[]`)}}}}}
	if err := arrayArguments.ValidateToolSequence(); err == nil || !strings.Contains(err.Error(), "JSON object") {
		t.Fatalf("non-object tool arguments were accepted: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Plan multipliers (GLM golden hours) + quota refresh

func TestCurrentMultiplier(t *testing.T) {
	p := &Plan{
		Timezone: "Asia/Shanghai",
		Windows: []MultiplierWindow{
			{Start: "00:00", End: "08:00", Multiplier: 0.5},
		},
	}
	// 03:00 Shanghai == inside the 00:00-08:00 window
	inWin := time.Date(2026, 8, 21, 3, 30, 0, 0, time.FixedZone("CST", 8*3600))
	if got := p.CurrentMultiplier(inWin); got != 0.5 {
		t.Errorf("overnight window: got %v want 0.5", got)
	}
	outWin := time.Date(2026, 8, 21, 12, 0, 0, 0, inWin.Location())
	if got := p.CurrentMultiplier(outWin); got != 1.0 {
		t.Errorf("outside window: got %v want 1.0", got)
	}
	// wrap-midnight window 22:00-06:00 (half-open [s,e) per segment);
	// no timezone on this plan -> windows evaluate in UTC
	wrap := &Plan{Windows: []MultiplierWindow{{Start: "22:00", End: "06:00", Multiplier: 0.3}}}
	late := time.Date(2026, 8, 21, 23, 0, 0, 0, time.UTC)
	early := time.Date(2026, 8, 21, 5, 59, 0, 0, time.UTC)
	boundary := time.Date(2026, 8, 21, 6, 0, 0, 0, time.UTC)
	noon := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	if got := wrap.CurrentMultiplier(late); got != 0.3 {
		t.Errorf("wrap late: %v", got)
	}
	if got := wrap.CurrentMultiplier(early); got != 0.3 {
		t.Errorf("wrap early: %v", got)
	}
	if got := wrap.CurrentMultiplier(boundary); got != 1.0 {
		t.Errorf("wrap at end boundary is excluded: %v", got)
	}
	if got := wrap.CurrentMultiplier(noon); got != 1.0 {
		t.Errorf("wrap noon: %v", got)
	}
	var nilPlan *Plan
	if nilPlan.CurrentMultiplier(time.Now()) != 1.0 {
		t.Error("nil plan multiplier must be 1.0")
	}
	bad := &Plan{Windows: []MultiplierWindow{{Start: "xx", End: "yy", Multiplier: 9}}}
	if bad.CurrentMultiplier(noon) != 1.0 {
		t.Error("invalid windows are skipped -> 1.0")
	}
}

func TestNextRefreshDaily(t *testing.T) {
	q := &QuotaBucket{Refresh: RefreshSpec{Period: "daily", At: "04:00", Timezone: "Asia/Shanghai"}}
	sh := time.FixedZone("CST", 8*3600)
	before := time.Date(2026, 8, 21, 2, 0, 0, 0, sh)
	got := q.NextRefresh(before)
	want := time.Date(2026, 8, 21, 4, 0, 0, 0, sh)
	if !got.Equal(want) {
		t.Errorf("before reset: got %v want %v", got, want)
	}
	after := time.Date(2026, 8, 21, 5, 0, 0, 0, sh)
	got = q.NextRefresh(after)
	want = want.AddDate(0, 0, 1)
	if !got.Equal(want) {
		t.Errorf("after reset: got %v want %v", got, want)
	}
	var nilQ *QuotaBucket
	if !nilQ.NextRefresh(before).Equal(before) {
		t.Error("nil bucket returns now")
	}
}

func TestNextRefreshWeeklyMonthlyDuration(t *testing.T) {
	utc := time.UTC
	now := time.Date(2026, 8, 21, 10, 0, 0, 0, utc) // Friday
	wk := &QuotaBucket{Refresh: RefreshSpec{Period: "weekly", At: "00:00"}}
	got := wk.NextRefresh(now)
	// next Monday after Fri Aug 21 2026 is Aug 24
	if got.Weekday() != time.Monday || got.Day() != 24 {
		t.Errorf("weekly: got %v (%s)", got, got.Weekday())
	}
	mo := &QuotaBucket{Refresh: RefreshSpec{Period: "monthly", At: "09:00"}}
	got = mo.NextRefresh(now)
	if got.Month() != time.September || got.Day() != 1 {
		t.Errorf("monthly: got %v", got)
	}
	dur := &QuotaBucket{Refresh: RefreshSpec{Period: "24h"}}
	got = dur.NextRefresh(now)
	// Duration periods are epoch-anchored: next = ceil(now, 24h), so it may
	// land anywhere in (now, now+24h].
	if !got.After(now) || got.After(now.Add(24*time.Hour)) {
		t.Errorf("duration 24h: got %v from %v", got, now)
	}
	badDur := &QuotaBucket{Refresh: RefreshSpec{Period: "nonsense"}}
	if got := badDur.NextRefresh(now); !got.Equal(now.Add(24 * time.Hour)) {
		t.Errorf("invalid duration falls back to +24h: got %v", got)
	}
}

// ---------------------------------------------------------------------------
// State helpers

func TestQuotaHelpers(t *testing.T) {
	var nilSt *TargetState
	if nilSt.QuotaRemainingFrac() != 1 {
		t.Error("nil state frac = 1")
	}
	if nilSt.QuotaExhausted() {
		t.Error("nil state never exhausted")
	}
	st := &TargetState{QuotaUsed: 250, QuotaLimit: 1000}
	if got := st.QuotaRemainingFrac(); got != 0.75 {
		t.Errorf("frac = %v want 0.75", got)
	}
	if st.QuotaExhausted() {
		t.Error("not exhausted at 75%")
	}
	st.QuotaUsed = 1000
	if !st.QuotaExhausted() {
		t.Error("exhausted at limit")
	}
	if got := st.QuotaRemainingFrac(); got != 0 {
		t.Errorf("frac clamps to 0, got %v", got)
	}
	st.QuotaUsed = 1200
	if got := st.QuotaRemainingFrac(); got != 0 {
		t.Errorf("overuse clamps to 0, got %v", got)
	}
	free := &TargetState{}
	if free.QuotaExhausted() || free.QuotaRemainingFrac() != 1 {
		t.Error("limit 0 means unknown/unlimited, never exhausted")
	}
}

func TestAvailableAt(t *testing.T) {
	now := time.Date(2026, 8, 21, 12, 0, 0, 0, time.UTC)
	var nilSt *TargetState
	if !nilSt.AvailableAt(now).IsZero() {
		t.Error("nil state available now")
	}
	cd := now.Add(5 * time.Minute)
	st := &TargetState{CooldownUntil: cd}
	if !st.AvailableAt(now).Equal(cd) {
		t.Error("cooldown wins when alone")
	}
	open := now.Add(10 * time.Minute)
	st.OpenUntil, st.Breaker = open, BreakerOpen
	if !st.AvailableAt(now).Equal(open) {
		t.Error("later OpenUntil wins")
	}
	pastCd := now.Add(-time.Minute)
	st.CooldownUntil = pastCd
	if !st.AvailableAt(now).Equal(open) {
		t.Error("past cooldown ignored")
	}
}

func TestSnapshotGet(t *testing.T) {
	now := time.Now()
	st := TargetState{ConsecFails: 2}
	snap := NewSnapshot(now, map[ResourceTargetID]*TargetState{"a": &st})
	// NewSnapshot deep-copies the input map values.
	st.ConsecFails = 99
	if snap.Targets["a"].ConsecFails != 2 {
		t.Fatal("NewSnapshot must deep-copy state at construction")
	}
	got := snap.Get("a")
	if got != snap.Targets["a"] {
		t.Error("Get returns the stored state for existing targets")
	}
	def := snap.Get("missing")
	if def.QuotaConf != ConfHigh {
		t.Error("missing target defaults to high-confidence fresh state")
	}
}

// ---------------------------------------------------------------------------
// Cost computation

func TestRateCardCost(t *testing.T) {
	var nilCard *RateCard
	if nilCard.Cost(Usage{InputTokens: 100}) != 0 {
		t.Error("nil card costs nothing")
	}
	card := &RateCard{InputPerM: 2, OutputPerM: 8, CacheReadPerM: 0.2, CacheWritePerM: 4}
	u := Usage{InputTokens: 1_000_000, OutputTokens: 500_000, CacheReadTokens: 1_000_000}
	got := card.Cost(u)
	want := 2.0 + 4.0 + 0.2
	if got != want {
		t.Errorf("Cost=%v want %v", got, want)
	}
}

func TestQuotaCost(t *testing.T) {
	u := Usage{InputTokens: 100, OutputTokens: 50}
	if got := QuotaCost(QuotaUnitPrompts, u, 0.5, true, 1); got != 0.5 {
		t.Errorf("prompts = mult, got %v", got)
	}
	if got := QuotaCost(QuotaUnitPrompts, u, 0.5, false, 1); got != 0 {
		t.Errorf("prompts with isRequest=false must be 0, got %v", got)
	}
	if got := QuotaCost(QuotaUnitTokens, u, 2, true, 1); got != 300 {
		t.Errorf("tokens = total×mult, got %v", got)
	}
	if got := QuotaCost(QuotaUnitUSD, u, 2, true, 3); got != 6 {
		t.Errorf("usd = cash×mult, got %v", got)
	}
	if got := QuotaCost(QuotaUnit("other"), u, 2, true, 1); got != 300 {
		t.Errorf("default = total×mult, got %v", got)
	}
}

// ---------------------------------------------------------------------------
// Misc

func TestSortIDs(t *testing.T) {
	got := SortIDs([]ResourceTargetID{"glm-b", "glm-a", "kimi"})
	want := []ResourceTargetID{"glm-a", "glm-b", "kimi"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("got %v want %v", got, want)
		}
	}
}

func TestErrNoCandidateError(t *testing.T) {
	e := &ErrNoCandidate{Alias: "glm-4", Reason: "all excluded"}
	want := `no eligible resource for alias "glm-4": all excluded`
	if e.Error() != want {
		t.Errorf("got %q want %q", e.Error(), want)
	}
}

func TestUsageTotal(t *testing.T) {
	u := Usage{InputTokens: 10, OutputTokens: 32}
	if u.Total() != 42 {
		t.Errorf("Total=%d want 42", u.Total())
	}
}
