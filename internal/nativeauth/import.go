package nativeauth

import (
	"encoding/json"
	"fmt"
	"strings"
)

// ImportNewClaude stores an explicitly supplied Claude OAuth grant without any
// network request. Other drivers require different client/endpoint bindings.
func (m *Manager) ImportNewClaude(account string, token Token) (View, error) {
	if m.standby {
		return View{}, ErrStandby
	}
	_, ref, err := m.validate("claude", account)
	if err != nil {
		return View{}, err
	}
	for _, v := range []string{token.AccessToken, token.RefreshToken} {
		if v == "" || len(v) > 64<<10 || strings.ContainsAny(v, "\r\n") {
			return View{}, fmt.Errorf("invalid Claude OAuth credential")
		}
	}
	unlock := m.lock(ref)
	defer unlock()
	binding, err := randomValue()
	if err != nil {
		return View{}, err
	}
	token.BindingID = ""
	token.Extra = nil
	record := accountRecord{BindingID: binding, Token: &token, ImportedDormant: true}
	raw, err := json.Marshal(record)
	if err != nil {
		return View{}, err
	}
	if err = m.store.SetIfAbsent(ref, string(raw)); err != nil {
		return View{}, err
	}
	return m.view("claude", account, record), nil
}
