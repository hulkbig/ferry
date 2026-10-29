package config

import (
	"bytes"
	"fmt"
	"io"

	"gopkg.in/yaml.v3"
)

// ParseStartupBootstrap parses an already bounded, securely read bootstrap
// snapshot. Keep the same defaults and template expansion as the legacy Load
// path without reopening a staged credential-bearing file by pathname.
func ParseStartupBootstrap(raw []byte) (*File, error) {
	var f File
	decoder := yaml.NewDecoder(bytes.NewReader(raw))
	decoder.KnownFields(true)
	if err := decoder.Decode(&f); err != nil {
		return nil, fmt.Errorf("invalid bootstrap configuration")
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("bootstrap configuration must contain one YAML document")
	}
	f.applyDefaults()
	if err := f.expandPlanTemplates(); err != nil {
		return nil, fmt.Errorf("invalid bootstrap plan configuration")
	}
	if err := f.Validate(); err != nil {
		// Validation errors can quote caller-provided values. Do not expose
		// bootstrap material through the startup log.
		return nil, fmt.Errorf("invalid bootstrap configuration")
	}
	return &f, nil
}
