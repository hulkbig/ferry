package domain

import "time"

// defaultSoftRecoveryWindow remains an internal baseline only. Adaptive
// Recovery Budget sizing is a separate follow-up and must not become a V2 API
// contract through this constant.
const defaultSoftRecoveryWindow = 30 * time.Second

// RequestOutcome is the client-visible logical result independent from the
// number of upstream attempts needed to produce it.
type RequestOutcome string

const (
	OutcomeDirectSuccess    RequestOutcome = "DIRECT_SUCCESS"
	OutcomeRecoveredSuccess RequestOutcome = "RECOVERED_SUCCESS"
	OutcomeFailed           RequestOutcome = "FAILED"
)

// RecoveryBudget bounds serial recovery along independent dimensions. It is
// request-local mutable state and is never shared between snapshots.
type RecoveryBudget struct {
	HardDeadline         time.Time `json:"hard_deadline"`
	SoftRecoveryDeadline time.Time `json:"soft_recovery_deadline"`
	AttemptLimit         int       `json:"attempt_limit"`
	RouteLimit           int       `json:"route_limit"`
	AttemptsUsed         int       `json:"attempts_used"`
	RoutesUsed           int       `json:"routes_used"`
}

// DefaultRecoveryBudget provides bounded P1 serial recovery. The caller's
// context deadline may shorten the hard deadline.
func DefaultRecoveryBudget(now, contextDeadline time.Time, routeSteps int) RecoveryBudget {
	hard := now.Add(2 * time.Minute)
	if !contextDeadline.IsZero() && contextDeadline.Before(hard) {
		hard = contextDeadline
	}
	soft := now.Add(defaultSoftRecoveryWindow)
	if hard.Before(soft) {
		soft = hard
	}
	routeLimit := routeSteps - 1
	if routeLimit < 0 {
		routeLimit = 0
	}
	return RecoveryBudget{
		HardDeadline: hard, SoftRecoveryDeadline: soft,
		AttemptLimit: 8, RouteLimit: routeLimit,
	}
}

func (b *RecoveryBudget) AllowAttempt(now time.Time) bool {
	return b != nil && now.Before(b.HardDeadline) && b.AttemptsUsed < b.AttemptLimit
}

func (b *RecoveryBudget) ConsumeAttempt() { b.AttemptsUsed++ }

func (b *RecoveryBudget) AllowRecovery(now time.Time, routeAdvance bool) bool {
	if b == nil || !now.Before(b.HardDeadline) || !now.Before(b.SoftRecoveryDeadline) ||
		b.AttemptsUsed >= b.AttemptLimit {
		return false
	}
	return !routeAdvance || b.RoutesUsed < b.RouteLimit
}

func (b *RecoveryBudget) ConsumeRoute() { b.RoutesUsed++ }
