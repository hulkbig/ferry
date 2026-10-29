package domain

import (
	"testing"
	"time"
)

func TestRecoveryBudgetBoundsAttemptsRoutesAndTime(t *testing.T) {
	now := time.Date(2026, 8, 26, 12, 0, 0, 0, time.UTC)
	budget := DefaultRecoveryBudget(now, now.Add(10*time.Second), 3)
	if !budget.HardDeadline.Equal(now.Add(10*time.Second)) || !budget.SoftRecoveryDeadline.Equal(now.Add(10*time.Second)) || budget.RouteLimit != 2 {
		t.Fatalf("budget = %+v", budget)
	}
	for range budget.AttemptLimit {
		if !budget.AllowAttempt(now) {
			t.Fatal("attempt rejected before limit")
		}
		budget.ConsumeAttempt()
	}
	if budget.AllowAttempt(now) || budget.AllowRecovery(now, false) {
		t.Fatal("attempt exhaustion must stop recovery")
	}

	routes := DefaultRecoveryBudget(now, time.Time{}, 2)
	if !routes.AllowRecovery(now, true) {
		t.Fatal("first route advance rejected")
	}
	routes.ConsumeRoute()
	if routes.AllowRecovery(now, true) || routes.AllowRecovery(now.Add(31*time.Second), false) {
		t.Fatal("route or soft deadline limit not enforced")
	}
}
