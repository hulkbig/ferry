package nativeauth

// RuntimeIdentity identifies a completed login without revealing credentials or
// refreshing tokens. Rotation preserves it; replacing an account changes it.
func (m *Manager) RuntimeIdentity(ref string) (string, error) {
	id, account, ok := ParseRef(ref)
	if !ok {
		return "", ErrLoginRequired
	}
	if _, _, err := m.validate(id, account); err != nil {
		return "", err
	}
	unlock := m.lock(ref)
	defer unlock()
	r, err := m.read(ref)
	if err != nil {
		return "", err
	}
	if r.Token == nil || r.BindingID == "" {
		return "", ErrLoginRequired
	}
	return r.BindingID, nil
}
