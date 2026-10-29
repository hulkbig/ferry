package egress

import (
	"encoding/json"
	"fmt"

	"ferry/internal/domain"
)

func normalizeCodexServiceTier(body map[string]json.RawMessage) error {
	raw, ok := body["service_tier"]
	if !ok {
		return nil
	}
	var value string
	if err := json.Unmarshal(raw, &value); err != nil {
		return fmt.Errorf("service_tier must be a string")
	}
	tier, err := domain.NormalizeServiceTier(value)
	if err != nil {
		return err
	}
	if tier == "fast" {
		tier = "priority"
	}
	if tier == "" {
		delete(body, "service_tier")
	} else {
		body["service_tier"], _ = json.Marshal(tier)
	}
	return nil
}

// Strip only protocol cache hints. Do not recurse into tool arguments, JSON
// schemas or arbitrary user objects whose property names may be identical.
func stripCodexCacheBreakpoints(body map[string]json.RawMessage) {
	var items []json.RawMessage
	if json.Unmarshal(body["input"], &items) != nil {
		return
	}
	for i, raw := range items {
		var item map[string]json.RawMessage
		if json.Unmarshal(raw, &item) != nil || item == nil {
			continue
		}
		delete(item, "prompt_cache_breakpoint")
		for _, field := range []string{"content", "output"} {
			var parts []json.RawMessage
			if json.Unmarshal(item[field], &parts) != nil || parts == nil {
				continue
			}
			for j, rawPart := range parts {
				var part map[string]json.RawMessage
				if json.Unmarshal(rawPart, &part) != nil || part == nil {
					continue
				}
				delete(part, "prompt_cache_breakpoint")
				parts[j], _ = json.Marshal(part)
			}
			item[field], _ = json.Marshal(parts)
		}
		items[i], _ = json.Marshal(item)
	}
	if items != nil {
		body["input"], _ = json.Marshal(items)
	}
}
