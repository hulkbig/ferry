package scheduler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"ferry/internal/domain"
	"ferry/internal/egress"
)

// compatibilityAssessor retains only small diagnostics for this request and
// world. Payloads are never cached across requests, credentials or protocols.
// Planners call assess only after route, capability and availability gates.
type compatibilityAssessor struct {
	ctx                         context.Context
	w                           *world
	rc                          *domain.RequestContract
	allowDegradation, exactOnly bool
	results                     map[domain.ResourceTargetID]compatAssessment
	prepared, cacheHits         int
	duration                    time.Duration
}

func newCompatibilityAssessor(ctx context.Context, w *world, rc *domain.RequestContract, allowDegradation, exactOnly bool) *compatibilityAssessor {
	return &compatibilityAssessor{ctx: ctx, w: w, rc: rc, allowDegradation: allowDegradation, exactOnly: exactOnly,
		results: make(map[domain.ResourceTargetID]compatAssessment)}
}

func (c *compatibilityAssessor) assess(id domain.ResourceTargetID) compatAssessment {
	if err := c.ctx.Err(); err != nil {
		return compatAssessment{level: domain.CompatUnsupported, blocked: true, reason: err.Error()}
	}
	if result, ok := c.results[id]; ok {
		c.cacheHits++
		return result
	}
	result := c.prepare(id)
	c.results[id] = result
	return result
}

func planningCancellation(ctx context.Context) *domain.Failure {
	return &domain.Failure{Type: domain.FailClientCancel, Scope: domain.FailureScopeRequest,
		Message: "request canceled during planning: " + ctx.Err().Error()}
}

func (c *compatibilityAssessor) prepare(id domain.ResourceTargetID) compatAssessment {
	t := c.w.snap.Targets[id]
	if t == nil {
		return compatAssessment{level: domain.CompatUnsupported, blocked: true, reason: "target is unavailable"}
	}
	if !t.AcceptsProtocol(c.rc.Protocol) {
		return compatAssessment{level: domain.CompatUnsupported, blocked: true,
			reason: "protocol conversion disabled for " + string(c.rc.Protocol)}
	}
	caller, ok := c.w.callers[id]
	if !ok {
		return compatAssessment{level: domain.CompatUnsupported, blocked: true,
			reason: "no egress caller configured"}
	}
	p, ok := caller.(egress.Preparer)
	if !ok {
		return compatAssessment{level: domain.CompatUnsupported, blocked: true,
			reason: "adapter cannot preview protocol conversion"}
	}
	contract, mappingNotes := contractForTarget(c.rc, t)
	c.prepared++
	started := time.Now()
	preparerNotes, err := p.Prepare(contract)
	c.duration += time.Since(started)
	if err != nil {
		return compatAssessment{level: domain.CompatUnsupported, blocked: true,
			reason: "protocol incompatible: " + err.Error()}
	}
	notes := append(mappingNotes, preparerNotes...)
	level := domain.CompatMapped
	if len(notes) == 0 {
		level = domain.CompatMapped
		if t.SpeaksNatively(c.rc.Protocol) {
			level = domain.CompatExact
		}
	} else {
		level = worstNoteLevel(notes)
	}
	// Auxiliary native features (caching hints, context editing,
	// verbosity, provider sampling extras) are intentional translations,
	// not degradations: they are reported to the client but never
	// affect the compatibility level.
	notes = append(notes, c.rc.MappingNotes...)
	a := compatAssessment{level: level, notes: notes}
	if c.exactOnly && level != domain.CompatExact {
		a.blocked = true
		detail := strings.Join(notes, "; ")
		if detail == "" && level == domain.CompatMapped {
			detail = "protocol conversion required"
		}
		a.reason = fmt.Sprintf("compatibility %s not allowed by exact_only policy: %s", level, detail)
	} else if !domain.DegradationAllowed(c.allowDegradation, level) {
		a.blocked = true
		if level == domain.CompatUnsupported {
			a.reason = "compatibility unsupported"
		} else {
			a.reason = fmt.Sprintf("compatibility %s not allowed (zero silent degradation): %s",
				level, strings.Join(notes, "; "))
		}
	}
	return a
}
