package scheduler

// Opt-in, offline planner regression. Baseline measurements for 0ffbb1d are
// preserved in docs/diagnostics/2026-09-29-pending-preflight.md. The topology
// mirrors revision 433 counts; all request data and credentials are synthetic.
// FERRY_PENDING_DIAGNOSTIC=1 go test ./internal/scheduler -run '^TestPendingPreflightDiagnostic$' -v -count=1 -timeout=10m

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/egress"
	schedulerRuntime "ferry/internal/runtime"
)

type pendingPreparationStats struct {
	calls              int
	errors             int
	byProtocol         map[domain.Protocol]int
	durationByProtocol map[domain.Protocol]time.Duration
}

type pendingDiagnosticCaller struct {
	preparer egress.Preparer
	stats    *pendingPreparationStats
}

func (c pendingDiagnosticCaller) Invoke(context.Context, *domain.RequestContract) (*domain.Response, *domain.Failure) {
	panic("offline diagnostic must never execute upstream")
}

func (c pendingDiagnosticCaller) Stream(context.Context, *domain.RequestContract) (<-chan domain.StreamEvent, func(), *domain.Failure) {
	panic("offline diagnostic must never execute upstream")
}

func (c pendingDiagnosticCaller) Prepare(rc *domain.RequestContract) ([]string, error) {
	start := time.Now()
	notes, err := c.preparer.Prepare(rc)
	c.stats.calls++
	c.stats.byProtocol[rc.Protocol]++
	c.stats.durationByProtocol[rc.Protocol] += time.Since(start)
	if err != nil {
		c.stats.errors++
	}
	return notes, err
}

func pendingDiagnosticWorld(stats *pendingPreparationStats) *world {
	w := &world{snap: &domain.CompiledSnapshot{V2DomainSource: config.V2DomainSourceNative, Targets: map[domain.ResourceTargetID]*domain.CompiledTarget{}, Providers: map[string]*domain.Provider{}, Connections: map[string]*domain.Connection{}}, callers: map[domain.ResourceTargetID]egress.Caller{}}
	add := func(connection string, proto domain.Protocol, count int, enabled, codex, tools, parallel bool) {
		provider := strings.Split(connection, "-")[0]
		w.snap.Providers[provider] = &domain.Provider{ID: provider, Enabled: true}
		w.snap.Connections[connection] = &domain.Connection{ID: connection, ProviderID: provider, Enabled: enabled}
		for i := 0; i < count; i++ {
			id := domain.ResourceTargetID(fmt.Sprintf("%s-%s-%t-%04d", connection, proto, parallel, i))
			target := &domain.CompiledTarget{ID: id, Model: fmt.Sprintf("synthetic-%s-%t-%04d", proto, parallel, i), ProviderID: provider, ConnectionID: connection, Disabled: !enabled,
				Endpoint:        &domain.Endpoint{BaseURL: "http://127.0.0.1:1", Protocol: proto},
				NativeProtocols: []domain.Protocol{proto}, Capabilities: domain.Capabilities{Text: true, Vision: true, Stream: true, Tools: tools, ParallelTools: parallel},
				ProtocolConversion: map[domain.Protocol]bool{domain.ProtocolOpenAIChat: true, domain.ProtocolOpenAIResponses: true, domain.ProtocolAnthropic: true}}
			w.snap.TargetOrder = append(w.snap.TargetOrder, id)
			w.snap.Targets[id] = target
			// Production publish does not create callers for disabled targets.
			if !enabled {
				continue
			}
			base := egress.Base{Endpoint: target.Endpoint, Model: target.Model}
			caller := egress.NewCaller(base, proto)
			if codex && proto == domain.ProtocolOpenAIResponses {
				caller = egress.NewCodexCaller(base, "synthetic-account")
			}
			w.callers[id] = pendingDiagnosticCaller{preparer: caller.(egress.Preparer), stats: stats}
		}
	}
	for _, proto := range []domain.Protocol{domain.ProtocolOpenAIChat, domain.ProtocolOpenAIResponses, domain.ProtocolAnthropic} {
		add("kimi", proto, 4, true, false, true, false)
		add("deepseek", proto, 4, false, false, true, false)
		add("zhipu", proto, 11, true, false, true, false)
		add("doubao", proto, 4, true, false, true, false)
		add("openrouter", proto, 414, true, false, true, true)
		withoutTools := 70
		if proto == domain.ProtocolAnthropic {
			withoutTools = 479
		}
		add("openrouter", proto, withoutTools, true, false, false, false)
	}
	add("codex-example", domain.ProtocolOpenAIResponses, 7, false, true, true, true)
	add("codex-example", domain.ProtocolOpenAIImages, 1, false, true, false, false)
	add("codex-nana", domain.ProtocolOpenAIResponses, 9, true, true, true, true)
	add("codex-nana", domain.ProtocolOpenAIImages, 1, true, true, false, false)
	return w
}

func pendingDiagnosticRequest(t *testing.T) *domain.RequestContract {
	t.Helper()
	text := strings.Repeat("synthetic offline input. ", 6000)
	image := make([]byte, 5311000)
	input := []any{map[string]any{"role": "user", "content": []any{
		map[string]any{"type": "input_text", "text": text},
		map[string]any{"type": "input_image", "image_url": "data:image/png;base64," + base64.StdEncoding.EncodeToString(image)},
	}}}
	native, err := json.Marshal(map[string]any{"model": "gpt-6-astra", "stream": true, "parallel_tool_calls": true, "input": input})
	if err != nil {
		t.Fatal(err)
	}
	parallel := true
	return &domain.RequestContract{Protocol: domain.ProtocolOpenAIResponses, Origin: domain.ProtocolOpenAIResponses,
		Model: "gpt-6-astra", Stream: true, ParallelTools: &parallel, Native: native,
		Messages: []domain.Message{{Role: domain.RoleUser, Blocks: []domain.Block{
			{Kind: domain.BlockText, Text: text}, {Kind: domain.BlockImage, ImageMIME: "image/png", ImageData: image},
		}}}}
}

func TestPendingPreflightDiagnostic(t *testing.T) {
	if os.Getenv("FERRY_PENDING_DIAGNOSTIC") != "1" {
		t.Skip("opt-in offline diagnostic")
	}
	rc := pendingDiagnosticRequest(t)
	t.Logf("go=%s native_bytes=%d synthetic_image_bytes=%d", runtime.Version(), len(rc.Native), len(rc.Messages[0].Blocks[1].ImageData))
	for _, nativeOnly := range []bool{false, true} {
		t.Run(fmt.Sprintf("full_catalog_native_only_%t", nativeOnly), func(t *testing.T) {
			stats := &pendingPreparationStats{byProtocol: map[domain.Protocol]int{}, durationByProtocol: map[domain.Protocol]time.Duration{}}
			w := pendingDiagnosticWorld(stats)
			if len(w.snap.TargetOrder) != 1948 {
				t.Fatalf("targets=%d", len(w.snap.TargetOrder))
			}
			var upstream string
			for _, id := range w.snap.TargetOrder {
				if target := w.snap.Targets[id]; target.ConnectionID == "codex-nana" && target.SpeaksNatively(domain.ProtocolOpenAIResponses) {
					upstream = target.Model
					break
				}
			}
			w.snap.LogicalModels = map[string]*domain.LogicalModel{
				"gpt-6-astra": {ID: "gpt-6-astra", Enabled: true, DefaultRoute: domain.RouteSpec{Steps: []domain.RouteStep{{ProviderID: "codex", UpstreamModel: upstream}}}},
			}
			sch := New(schedulerRuntime.NewManager(schedulerRuntime.DefaultHealthConfig()), openTempLedger(t).Ledger)
			sch.ReplaceWorld(w.snap, w.callers, nil, []config.ConsumerKey{{Name: "diagnostic", Key: "synthetic-key"}})

			request := *rc
			if nativeOnly {
				request.ConversionIssues = []string{"synthetic native-only feature"}
			}
			runtime.GC()
			var before, after runtime.MemStats
			runtime.ReadMemStats(&before)
			start := time.Now()
			result, failure := sch.decide(context.Background(), &request, RequestMeta{BearerToken: "synthetic-key"}, true)
			if failure != nil {
				t.Fatal(failure)
			}
			elapsed := time.Since(start)
			runtime.ReadMemStats(&after)
			t.Logf("targets=%d prepare_calls=%d errors=%d elapsed=%s allocated_bytes=%d allocations=%d gc=%d calls_by_protocol=%v duration_by_protocol=%v",
				len(w.snap.TargetOrder), stats.calls, stats.errors, elapsed, after.TotalAlloc-before.TotalAlloc, after.Mallocs-before.Mallocs, after.NumGC-before.NumGC, stats.byProtocol, stats.durationByProtocol)
			if stats.calls != 1 || len(result.compat) != 1 || len(result.plan.Order) != 1 {
				t.Fatalf("prepare=%d assessments=%d order=%v, want only one enabled routed target", stats.calls, len(result.compat), result.plan.Order)
			}

		})
	}
}
