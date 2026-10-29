package domain

import "testing"

func TestOAuthRefRoundTrip(t *testing.T) {
	ref := OAuthRef("my-account")
	if ref != "oauth:codex:my-account" {
		t.Fatalf("OAuthRef = %q", ref)
	}
	acct, ok := OAuthAccountFromRef(ref)
	if !ok || acct != "my-account" {
		t.Fatalf("OAuthAccountFromRef(%q) = %q,%v", ref, acct, ok)
	}
}

func TestOAuthAccountFromRefRejects(t *testing.T) {
	cases := []string{
		"", "store:foo", "env:foo", "oauth:", "oauth:codex:",
		"oauth:codex:-bad", "oauth:codex:has.dot", "oauth:codex:has space",
	}
	for _, ref := range cases {
		if _, ok := OAuthAccountFromRef(ref); ok {
			t.Errorf("OAuthAccountFromRef(%q) accepted", ref)
		}
	}
}
