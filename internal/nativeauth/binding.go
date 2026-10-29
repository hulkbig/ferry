package nativeauth

import (
	"fmt"
	"net/url"
	"strings"
)

// ValidateBinding forbids sending a managed OAuth credential to an editable
// arbitrary upstream or using a wire format unsupported by its native driver.
func (m *Manager) ValidateBinding(ref, baseURL, protocol string) error {
	id, _, ok := ParseRef(ref)
	if !ok {
		return fmt.Errorf("invalid native credential reference")
	}
	_, ok = m.Driver(id)
	if !ok {
		return fmt.Errorf("native provider unavailable")
	}
	info, err := m.boundInfo(ref)
	if err != nil {
		return err
	}
	got, e := url.Parse(baseURL)
	want, _ := url.Parse(info.BaseURL)
	if e != nil || got.Scheme != "https" || got.User != nil || got.RawQuery != "" || got.Fragment != "" || got.Host != want.Host || strings.TrimRight(got.Path, "/") != strings.TrimRight(want.Path, "/") {
		return fmt.Errorf("native credential requires its registered provider endpoint")
	}
	if protocol != info.Protocol {
		return fmt.Errorf("native credential requires its registered wire protocol")
	}
	return nil
}
