package domain

import (
	"reflect"
	"strings"
	"testing"
)

func TestCapabilityRegistryCoversCapabilitiesAndRequirements(t *testing.T) {
	capabilityType := reflect.TypeOf(Capabilities{})
	all := make([]string, 0, capabilityType.NumField())
	boolean := make([]string, 0, capabilityType.NumField())
	for index := 0; index < capabilityType.NumField(); index++ {
		field := capabilityType.Field(index)
		name := strings.Split(field.Tag.Get("json"), ",")[0]
		all = append(all, name)
		if field.Type.Kind() == reflect.Bool {
			boolean = append(boolean, name)
		}
	}
	if !reflect.DeepEqual(CapabilityRegistry(), all) {
		t.Fatalf("registry=%v capabilities=%v", CapabilityRegistry(), all)
	}
	if !reflect.DeepEqual(BooleanCapabilityRegistry(), boolean) {
		t.Fatalf("boolean registry=%v capabilities=%v", BooleanCapabilityRegistry(), boolean)
	}

	requirementType := reflect.TypeOf(Requirements{})
	requirementFields := map[string]reflect.StructField{}
	for index := 0; index < requirementType.NumField(); index++ {
		field := requirementType.Field(index)
		if field.Type.Kind() == reflect.Bool {
			requirementFields[strings.Split(field.Tag.Get("json"), ",")[0]] = field
		}
	}
	for _, name := range BooleanCapabilityRegistry() {
		field, ok := requirementFields[name]
		if !ok {
			t.Fatalf("boolean capability %q has no Requirements field", name)
		}
		var capabilities Capabilities
		if !SetBooleanCapability(&capabilities, name, true) {
			t.Fatalf("cannot set capability %q", name)
		}
		if supported, known := BooleanCapability(capabilities, name); !known || !supported {
			t.Fatalf("cannot read capability %q", name)
		}

		var requirements Requirements
		if !SetRequirementCapability(&requirements, name, true) {
			t.Fatalf("cannot set requirement %q", name)
		}
		if !reflect.ValueOf(requirements).FieldByIndex(field.Index).Bool() {
			t.Fatalf("requirement %q mapped to wrong field", name)
		}
		if required, known := RequirementCapability(requirements, name); !known || !required {
			t.Fatalf("cannot read requirement %q", name)
		}
	}
}

func TestMergeRequirementsAndContainsUseCompleteRegistry(t *testing.T) {
	var left, right Requirements
	for index, name := range BooleanCapabilityRegistry() {
		if index%2 == 0 {
			SetRequirementCapability(&left, name, true)
		} else {
			SetRequirementCapability(&right, name, true)
		}
	}
	left.MinContext = 100
	right.MinContext = 200
	left.MinOutput = 300
	right.MinOutput = 150

	merged := MergeRequirements(left, right)
	for _, name := range BooleanCapabilityRegistry() {
		required, known := RequirementCapability(merged, name)
		if !known || !required {
			t.Fatalf("merged requirement missing %q", name)
		}
	}
	if merged.MinContext != 200 || merged.MinOutput != 300 {
		t.Fatalf("merged numeric requirements=%+v", merged)
	}
	if !merged.Contains(left) || !merged.Contains(right) {
		t.Fatalf("merged requirements do not contain inputs: %+v", merged)
	}
}

func TestMergeCapabilitiesUsesCompleteRegistry(t *testing.T) {
	var left, right Capabilities
	for index, name := range BooleanCapabilityRegistry() {
		if index%2 == 0 {
			SetBooleanCapability(&left, name, true)
		} else {
			SetBooleanCapability(&right, name, true)
		}
	}
	left.ContextWindow = 100
	right.ContextWindow = 200
	left.MaxOutput = 300
	right.MaxOutput = 150

	merged := MergeCapabilities(left, right)
	for _, name := range BooleanCapabilityRegistry() {
		supported, known := BooleanCapability(merged, name)
		if !known || !supported {
			t.Fatalf("merged capabilities missing %q", name)
		}
	}
	if merged.ContextWindow != 200 || merged.MaxOutput != 300 {
		t.Fatalf("merged numeric capabilities=%+v", merged)
	}
}

func TestEffectiveCapabilitiesOnlyMapsParallelToolsFromTools(t *testing.T) {
	upstream := Capabilities{
		Text: true, Images: true, Stream: true, Tools: true, ParallelTools: false,
		Reasoning: true, StructuredOutput: true, Vision: true,
		ContextWindow: 131072, MaxOutput: 8192,
	}
	effective := EffectiveCapabilities(upstream)
	if !effective.ParallelTools {
		t.Fatalf("tools-capable target did not expose parallel_tools mapping: %+v", effective)
	}
	effective.ParallelTools = upstream.ParallelTools
	if !reflect.DeepEqual(effective, upstream) {
		t.Fatalf("unverified capabilities were changed: effective=%+v upstream=%+v", effective, upstream)
	}

	inconsistent := EffectiveCapabilities(Capabilities{ParallelTools: true})
	if inconsistent.ParallelTools {
		t.Fatalf("parallel_tools cannot be effective without tools: %+v", inconsistent)
	}
}
