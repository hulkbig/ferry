package egress

import (
	"encoding/json"
	"math"
	"testing"

	"ferry/internal/domain"
)

func TestDecodeCacheUsageDialects(t *testing.T) {
	for _, tc := range []struct {
		name, raw                            string
		input, output, read, write, uncached int64
		subset                               bool
	}{
		{"responses read and write", `{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":150}}`, 1000, 200, 800, 150, 50, true},
		{"responses read only", `{"input_tokens":1000,"output_tokens":200,"input_tokens_details":{"cached_tokens":800}}`, 1000, 200, 800, 0, 200, true},
		{"creation alias", `{"input_tokens":4022,"output_tokens":462,"input_tokens_details":{"cache_creation_tokens":4019}}`, 4022, 462, 0, 4019, 3, true},
		{"chat write only", `{"prompt_tokens":1000,"completion_tokens":200,"prompt_tokens_details":{"cache_write_tokens":150}}`, 1000, 200, 0, 150, 850, true},
		{"canonical zero wins", `{"prompt_tokens":1000,"prompt_tokens_details":{"cache_write_tokens":0,"cache_creation_tokens":150}}`, 1000, 0, 0, 0, 1000, true},
		{"independent", `{"input_tokens":50,"output_tokens":200,"cache_read_input_tokens":800,"cache_creation_input_tokens":150}`, 50, 200, 800, 150, 50, false},
		{"duplicate representations", `{"input_tokens":50,"cache_read_input_tokens":800,"cache_creation_input_tokens":150,"input_tokens_details":{"cached_tokens":800,"cache_write_tokens":150}}`, 50, 0, 800, 150, 50, false},
		{"explicit independent zero wins", `{"input_tokens":50,"cache_read_input_tokens":0,"input_tokens_details":{"cached_tokens":40}}`, 50, 0, 0, 0, 50, false},
		{"negative counters", `{"input_tokens":-10,"output_tokens":-5,"input_tokens_details":{"cached_tokens":-20,"cache_write_tokens":-30}}`, 0, 0, 0, 0, 0, true},
		{"excess writes", `{"input_tokens":100,"input_tokens_details":{"cached_tokens":80,"cache_write_tokens":50}}`, 100, 0, 80, 20, 0, true},
		{"overflowing details", `{"input_tokens":9223372036854775807,"input_tokens_details":{"cached_tokens":9223372036854775807,"cache_write_tokens":9223372036854775807}}`, math.MaxInt64, 0, math.MaxInt64, 0, 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			u := decodeUsage(json.RawMessage(tc.raw))
			want := domain.Usage{InputTokens: tc.input, OutputTokens: tc.output, CacheReadTokens: tc.read, CacheWriteTokens: tc.write, CacheSubsetOfInput: tc.subset}
			if u != want || u.UncachedInput() != tc.uncached {
				t.Fatalf("usage=%+v uncached=%d; want=%+v uncached=%d", u, u.UncachedInput(), want, tc.uncached)
			}
		})
	}
}
