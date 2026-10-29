package config

import (
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"ferry/internal/domain"
)

func TestFirstOutputTimeoutValidation(t *testing.T) {
	for _, seconds := range []int{-1, 0, 1, 180, 7200, 7201, int(^uint(0) >> 1)} {
		t.Run(fmt.Sprint(seconds), func(t *testing.T) {
			file := &File{V2DomainSource: V2DomainSourceNative, LogicalModels: []domain.LogicalModel{{
				ID: "smart", FirstOutputTimeoutSeconds: seconds,
			}}}
			err := file.Validate()
			if seconds < 0 || seconds > 7200 {
				if err == nil || !strings.Contains(err.Error(), "first_output_timeout_seconds") {
					t.Fatalf("invalid timeout %d: %v", seconds, err)
				}
			} else if err != nil {
				t.Fatalf("valid timeout %d rejected: %v", seconds, err)
			}
		})
	}
}

func TestFirstOutputTimeoutConfigJSONCompatibility(t *testing.T) {
	for _, field := range []string{"", `,"first_output_timeout_seconds":0`, `,"first_output_timeout_seconds":180`} {
		var file File
		if err := json.Unmarshal([]byte(`{"v2_domain_source":"native","logical_models":[{"id":"smart","enabled":false`+field+`}]}`), &file); err != nil {
			t.Fatal(err)
		}
		if err := file.Validate(); err != nil {
			t.Fatal(err)
		}
		want := 0
		if strings.Contains(field, "180") {
			want = 180
		}
		if got := file.LogicalModels[0].FirstOutputTimeoutSeconds; got != want {
			t.Fatalf("decoded timeout=%d want=%d", got, want)
		}
		raw, err := json.Marshal(file)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(raw), "first_output_timeout_seconds") != (want != 0) {
			t.Fatalf("timeout omitempty compatibility lost: %s", raw)
		}
		var restored File
		if err := json.Unmarshal(raw, &restored); err != nil {
			t.Fatal(err)
		}
		if got := restored.LogicalModels[0].FirstOutputTimeoutSeconds; got != want {
			t.Fatalf("restored timeout=%d want=%d", got, want)
		}
	}
	for _, value := range []string{`0.5`, `"180"`, `7200.1`, `true`} {
		var model domain.LogicalModel
		if err := json.Unmarshal([]byte(`{"first_output_timeout_seconds":`+value+`}`), &model); err == nil {
			t.Fatalf("non-integer JSON value %s accepted", value)
		}
	}
}
