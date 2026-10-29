package domain

import "testing"

func TestRuntimeOwnershipExcludesLabelsAndIncludesIncarnationAndCredential(t *testing.T) {
	original := Connection{ID: "same-name", ProviderID: "p", RuntimeID: "unique-incarnation", RuntimeCredential: t.Name() + "/account-a", Identity: "display"}
	id := original.RuntimeTargetID("model")
	renamed := original
	renamed.Identity = "new display"
	renamed.Enabled = true
	if renamed.RuntimeTargetID("model") != id {
		t.Fatal("display or enable edit changed ownership")
	}
	for _, change := range []func(*Connection){
		func(c *Connection) { c.RuntimeID = "new-incarnation" },
		func(c *Connection) { c.RuntimeCredential = t.Name() + "/account-b" },
		func(c *Connection) { c.Credential.SecretRef = t.Name() + "/other" },
		func(c *Connection) { c.BaseURL = "https://new-endpoint.test" },
	} {
		changed := original
		change(&changed)
		if changed.RuntimeTargetID("model") == id {
			t.Fatal("new owner reused old target ID")
		}
	}
	if original.RuntimeTargetID("other-model") == id {
		t.Fatal("different models share cooldown ID")
	}
}
