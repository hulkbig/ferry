package buildinfo

import "testing"

func TestCurrentUsesInjectedIdentity(t *testing.T) {
	old := Current()
	Version, Commit, BuildTime, Source = "main-abcdef123456", "abcdef123456abcdef123456abcdef123456abcd", "2026-08-23T01:02:03Z", "https://example.org/ferry/source"
	t.Cleanup(func() {
		Version, Commit, BuildTime, Source = old.Version, old.Commit, old.BuildTime, old.Source
	})
	got := Current()
	if got.Service != "ferry" || got.Version != Version || got.Commit != Commit || got.BuildTime != BuildTime || got.Source != Source {
		t.Fatalf("Current() = %+v", got)
	}
}
