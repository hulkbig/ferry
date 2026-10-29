package oauth

import (
	"encoding/json"
	"testing"
)

func TestCatalogPriorityRequiresExplicitTier(t *testing.T) {
	for _, tc := range []struct {
		name, fields     string
		known, supported bool
	}{
		{"absent", ``, false, false},
		{"null", `,"service_tiers":null`, false, false},
		{"empty", `,"service_tiers":[]`, true, false},
		{"default-only", `,"service_tiers":[{"id":"default","name":"Fast"}]`, true, false},
		{"priority", `,"service_tiers":[{"id":"priority","name":"Fast"}]`, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var entry codexModelEntry
			if err := json.Unmarshal([]byte(`{"slug":"gpt-6-astra"`+tc.fields+`}`), &entry); err != nil {
				t.Fatal(err)
			}
			model := catalogModelFromEntry(entry)
			if (model.Priority != nil) != tc.known || (model.Priority != nil && *model.Priority != tc.supported) {
				t.Fatalf("priority=%v, want known=%v supported=%v", model.Priority, tc.known, tc.supported)
			}
		})
	}
}
