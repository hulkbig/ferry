package nativeauth

import "ferry/internal/secretstore"

func DefaultManager(store *secretstore.Store) *Manager {
	return DefaultManagerWithOptions(store, Options{})
}

func DefaultManagerWithOptions(store *secretstore.Store, options Options) *Manager {
	return NewManagerWithOptions(store, options, NewClaudeDriver(), NewKimiDriver(false), NewKimiDriver(true), NewXAIDriver(), NewMetaDriver(), NewVertexDriver(), NewAntigravityDriver(), NewDevinDriver())
}
func ValidateDefaultBinding(ref, baseURL, protocol string) error {
	m := DefaultManager(nil)
	id, _, _ := ParseRef(ref)
	if d, ok := m.Driver(id); ok {
		if dynamic, ok := d.(EndpointValidator); ok {
			return dynamic.ValidateEndpoint(baseURL, protocol)
		}
	}
	return m.ValidateBinding(ref, baseURL, protocol)
}
