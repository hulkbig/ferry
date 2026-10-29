package management

import (
	"net/http"
	"testing"
)

func TestUsageProtectionSettingsPersistAndRejectInvalid(t *testing.T) {
	f := newV2APIFixture(t)
	rec, settings := f.do(http.MethodGet, "/api/settings", nil)
	requireStatus(t, rec, http.StatusOK)
	p := settings[usageProtectionKey].(map[string]any)
	if p["max_used_percent"] != float64(98) || p["allow_credits"] != false {
		t.Fatal(p)
	}
	for _, value := range []any{0, -1, 101, "98"} {
		rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{usageProtectionKey: map[string]any{"max_used_percent": value}})
		requireStatus(t, rec, http.StatusBadRequest)
	}
	rec, _ = f.do(http.MethodPatch, "/api/settings", map[string]any{usageProtectionKey: map[string]any{"max_used_percent": 95.5, "allow_credits": true}})
	requireStatus(t, rec, http.StatusOK)
	fresh := New(f.server.deps)
	saved, err := fresh.usageProtectionPolicy()
	if err != nil || saved.MaxUsedPercent != 95.5 || !saved.AllowCredits {
		t.Fatalf("%+v %v", saved, err)
	}
	reset, err := fresh.automaticUsageResetPolicy()
	if err != nil || !reset.Enabled || reset.ThresholdMinutes != 20 {
		t.Fatal("paid credits changed reset coupons")
	}
	if err := f.control.SetRuntimeSetting(usageProtectionKey, `{"max_used_percent":0}`); err != nil {
		t.Fatal(err)
	}
	if _, err := fresh.usageProtectionPolicy(); err == nil {
		t.Fatal("corrupt settings accepted")
	}
}
