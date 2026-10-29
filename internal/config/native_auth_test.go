package config

import "testing"

func TestNativeCredentialCannotBeResolvedThroughStaticStore(t *testing.T) {
	calls := 0
	r := &Resolver{Store: func(string) (string, bool) { calls++; return "private-token-record", true }}
	for _, ref := range []string{"store:native-oauth:claude:account", "store:oauth:codex:account", "env:native-oauth:claude:account"} {
		if _, err := r.Resolve(ref); err == nil {
			t.Fatalf("reserved credential resolved: %s", ref)
		}
	}
	if calls != 0 {
		t.Fatal("reserved record was accessed")
	}
}
