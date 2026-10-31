package egress

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func BenchmarkChatResponseInspection(b *testing.B) {
	for _, size := range []int{1024, 65536, 262144} {
		body, _ := json.Marshal(map[string]any{"id": "chatcmpl-bench", "object": "chat.completion", "model": "model", "choices": []any{map[string]any{"index": 0, "message": map[string]string{"role": "assistant", "content": strings.Repeat("x", size)}, "finish_reason": "stop"}}, "usage": map[string]int{"prompt_tokens": 32, "completion_tokens": size / 4, "total_tokens": 32 + size/4}})
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for b.Loop() {
				_, changed, issues := inspectChatResponse(body)
				if changed || len(issues) != 0 {
					b.Fatal("standard response changed")
				}
			}
		})
	}
}
