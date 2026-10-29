package nativeauth

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"sort"
	"strings"
	"sync"
	"time"

	"ferry/internal/secretstore"
)

const prefix = "native-oauth:"

var accountPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,95}$`)

func safeNonRotatingRefresh(d Driver) bool {
	capability, ok := d.(NonRotatingRefresher)
	return ok && capability.RefreshIsNonRotating()
}

type accountRecord struct {
	ImportedDormant bool      `json:"imported_dormant,omitempty"`
	RefreshInFlight bool      `json:"refresh_in_flight,omitempty"`
	BindingID       string    `json:"binding_id,omitempty"`
	Token           *Token    `json:"token,omitempty"`
	Session         *Session  `json:"session,omitempty"`
	NextPoll        time.Time `json:"next_poll,omitempty"`
}
type View struct {
	BaseURL         string    `json:"base_url,omitempty"`
	Protocol        string    `json:"protocol,omitempty"`
	Driver          string    `json:"driver"`
	Account         string    `json:"account"`
	CredentialRef   string    `json:"credential_ref"`
	Status          string    `json:"status"`
	URL             string    `json:"url,omitempty"`
	UserCode        string    `json:"user_code,omitempty"`
	ExpiresAt       time.Time `json:"expires_at,omitempty"`
	IntervalSeconds int       `json:"interval_seconds,omitempty"`
}
type Manager struct {
	standby bool // immutable after construction
	store   *secretstore.Store
	client  *http.Client
	drivers map[string]Driver
	locks   sync.Map
	blocked sync.Map
}

func NewManager(store *secretstore.Store, drivers ...Driver) *Manager {
	return NewManagerWithOptions(store, Options{}, drivers...)
}

type Options struct {
	Standby bool
}

var ErrStandby = errors.New("upstream account changes are disabled in standby mode")

func NewManagerWithOptions(store *secretstore.Store, options Options, drivers ...Driver) *Manager {
	m := &Manager{store: store, client: &http.Client{Timeout: 30 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}, drivers: map[string]Driver{}}
	m.standby = options.Standby
	for _, d := range drivers {
		m.drivers[d.Info().ID] = d
	}
	return m
}
func Ref(driver, account string) string { return prefix + driver + ":" + account }
func ParseRef(ref string) (string, string, bool) {
	p := strings.Split(strings.TrimPrefix(ref, prefix), ":")
	if !strings.HasPrefix(ref, prefix) || len(p) != 2 || !accountPattern.MatchString(p[0]) || !accountPattern.MatchString(p[1]) {
		return "", "", false
	}
	return p[0], p[1], true
}
func (m *Manager) Drivers() []Info {
	out := []Info{}
	for _, d := range m.drivers {
		out = append(out, d.Info())
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	return out
}
func (m *Manager) Driver(id string) (Driver, bool) { d, ok := m.drivers[id]; return d, ok }
func (m *Manager) lock(ref string) func() {
	v, _ := m.locks.LoadOrStore(ref, &sync.Mutex{})
	mu := v.(*sync.Mutex)
	mu.Lock()
	return mu.Unlock
}
func (m *Manager) read(ref string) (accountRecord, error) {
	var r accountRecord
	raw, ok := m.store.Get(ref)
	if !ok {
		return r, nil
	}
	if json.Unmarshal([]byte(raw), &r) != nil {
		return r, fmt.Errorf("native account record unreadable")
	}
	return r, nil
}
func (m *Manager) save(ref string, r accountRecord) error {
	raw, e := json.Marshal(r)
	if e != nil {
		return e
	}
	return m.store.Set(ref, string(raw))
}
func (m *Manager) validate(driver, account string) (Driver, string, error) {
	d, ok := m.drivers[driver]
	if !ok || !accountPattern.MatchString(account) {
		return nil, "", fmt.Errorf("unsupported provider or invalid account")
	}
	return d, Ref(driver, account), nil
}
func randomValue() (string, error) {
	b := make([]byte, 32)
	if _, e := rand.Read(b); e != nil {
		return "", e
	}
	return base64.RawURLEncoding.EncodeToString(b), nil
}
func publicView(driver, account string, r accountRecord) View {
	v := View{Driver: driver, Account: account, CredentialRef: Ref(driver, account), Status: "disconnected"}
	if r.Token != nil {
		v.Status = "connected"
		if r.Token.AccessToken == "" {
			v.Status = "key_pending"
		}
		if r.RefreshInFlight {
			v.Status = "relogin_required"
		}
		v.ExpiresAt = r.Token.ExpiresAt
	}
	if r.Session != nil {
		v.Status = "pending"
		v.URL = r.Session.URL
		v.UserCode = r.Session.UserCode
		v.ExpiresAt = r.Session.ExpiresAt
		v.IntervalSeconds = r.Session.IntervalSeconds
		if !time.Now().Before(v.ExpiresAt) {
			v.Status = "expired"
		}
	}
	return v
}
func (m *Manager) Status(driver, account string) (View, error) {
	_, ref, e := m.validate(driver, account)
	if e != nil {
		return View{}, e
	}
	unlock := m.lock(ref)
	defer unlock()
	r, e := m.read(ref)
	return m.view(driver, account, r), e
}
func (m *Manager) Begin(ctx context.Context, driver, account string, fields map[string]string) (View, error) {
	if m.standby {
		return View{}, ErrStandby
	}
	d, ref, e := m.validate(driver, account)
	if e != nil {
		return View{}, e
	}
	unlock := m.lock(ref)
	defer unlock()
	r, e := m.read(ref)
	if e != nil {
		return View{}, e
	}
	state, e := randomValue()
	if e != nil {
		return View{}, e
	}
	verifier, e := randomValue()
	if e != nil {
		return View{}, e
	}
	session, e := d.Begin(ctx, m.client, LoginOptions{State: state, Verifier: verifier, Fields: fields})
	if e != nil {
		return View{}, fmt.Errorf("provider login could not start")
	}
	if session.ExpiresAt.IsZero() {
		session.ExpiresAt = time.Now().Add(15 * time.Minute)
	}
	r.Session = &session
	r.NextPoll = time.Now().Add(time.Duration(session.IntervalSeconds) * time.Second)
	if e = m.save(ref, r); e != nil {
		return View{}, e
	}
	return m.view(driver, account, r), nil
}
func (m *Manager) Complete(ctx context.Context, driver, account, callback string) (View, error) {
	if m.standby {
		return View{}, ErrStandby
	}
	d, ref, e := m.validate(driver, account)
	if e != nil {
		return View{}, e
	}
	unlock := m.lock(ref)
	defer unlock()
	r, e := m.read(ref)
	if e != nil {
		return View{}, e
	}
	if r.Session == nil {
		if driver == "meta" && r.Token != nil && r.Token.AccessToken == "" && r.Token.RefreshToken != "" {
			token, err := d.Refresh(ctx, m.client, *r.Token)
			if err != nil || token.AccessToken == "" {
				return View{}, fmt.Errorf("provider key issuance failed; retry explicitly")
			}
			if token.RefreshToken == "" {
				token.RefreshToken = r.Token.RefreshToken
			}
			r.Token = &token
			r.RefreshInFlight = false
			r.NextPoll = time.Time{}
			r.BindingID, err = randomValue()
			if err != nil {
				return View{}, err
			}
			if err = m.save(ref, r); err != nil {
				return View{}, err
			}
			m.blocked.Delete(ref)
			return m.view(driver, account, r), nil
		}
		return m.view(driver, account, r), fmt.Errorf("no pending login")
	}
	if !time.Now().Before(r.Session.ExpiresAt) {
		return m.view(driver, account, r), fmt.Errorf("login expired")
	}
	if d.Info().Mode == "device" && time.Now().Before(r.NextPoll) {
		return m.view(driver, account, r), nil
	}
	token, e := d.Complete(ctx, m.client, *r.Session, callback)
	if errors.Is(e, ErrPending) || errors.Is(e, ErrSlowDown) {
		if errors.Is(e, ErrSlowDown) {
			r.Session.IntervalSeconds += 5
		}
		if r.Session.IntervalSeconds < 1 {
			r.Session.IntervalSeconds = 5
		}
		r.NextPoll = time.Now().Add(time.Duration(r.Session.IntervalSeconds) * time.Second)
		if e = m.save(ref, r); e != nil {
			return View{}, e
		}
		return m.view(driver, account, r), nil
	}
	if e != nil {
		if driver == "meta" && token.AccessToken == "" && token.RefreshToken != "" {
			// A fresh device grant supersedes the old login, even when minting
			// its inference key failed. Do not carry the old binding or lock.
			r.Token = &token
			r.Session = nil
			r.BindingID = ""
			r.RefreshInFlight = false
			r.NextPoll = time.Time{}
			if err := m.save(ref, r); err != nil {
				return View{}, err
			}
			m.blocked.Delete(ref)
			return m.view(driver, account, r), nil
		}
		return View{}, fmt.Errorf("provider login failed; verify authorization and try again")
	}
	if token.AccessToken == "" {
		return View{}, fmt.Errorf("provider returned no credential")
	}
	r.BindingID, e = randomValue()
	if e != nil {
		return View{}, e
	}
	r.Token = &token
	r.ImportedDormant = false
	r.RefreshInFlight = false
	r.Session = nil
	r.NextPoll = time.Time{}
	if e = m.save(ref, r); e != nil {
		return View{}, e
	}
	m.blocked.Delete(ref)
	return m.view(driver, account, r), nil
}
func (m *Manager) Disconnect(driver, account string) error {
	_, ref, e := m.validate(driver, account)
	if e != nil {
		return e
	}
	unlock := m.lock(ref)
	defer unlock()
	m.blocked.Delete(ref)
	return m.store.Delete(ref)
}

// Identity serializes rotating refreshes; failed is the rejected access token,
// allowing concurrent 401s to reuse a refresh that already completed.
func (m *Manager) Identity(ctx context.Context, ref, failed string) (Token, error) {
	return m.identity(ctx, ref, failed, false)
}

func (m *Manager) identity(ctx context.Context, ref, failed string, background bool) (Token, error) {
	driver, account, ok := ParseRef(ref)
	if !ok {
		return Token{}, ErrLoginRequired
	}
	d, _, e := m.validate(driver, account)
	if e != nil {
		return Token{}, e
	}
	unlock := m.lock(ref)
	defer unlock()
	r, e := m.read(ref)
	if e != nil {
		return Token{}, e
	}
	nonRotating := safeNonRotatingRefresh(d)
	if r.Token == nil || r.Token.AccessToken == "" {
		return Token{}, ErrLoginRequired
	}
	if r.RefreshInFlight {
		if !nonRotating {
			return Token{}, ErrLoginRequired
		}
		// Recover a legacy/interrupted journal only for an explicitly safe
		// driver. The old service-account key remains usable after any outcome.
		r.RefreshInFlight = false
		if e = m.save(ref, r); e != nil {
			return Token{}, fmt.Errorf("cannot clear nonrotating refresh journal")
		}
	}
	t := *r.Token
	t.BindingID = r.BindingID
	if background && r.ImportedDormant {
		return t, nil
	}
	due := !t.ExpiresAt.IsZero() && time.Until(t.ExpiresAt) < 3*time.Minute
	if !due && (failed == "" || failed != t.AccessToken) {
		return t, nil
	}
	if m.standby {
		return Token{}, ErrStandby
	}
	if until, ok := m.blocked.Load(ref); ok && time.Now().Before(until.(time.Time)) {
		if nonRotating {
			return Token{}, fmt.Errorf("credential refresh temporarily unavailable; retry later")
		}
		return Token{}, ErrLoginRequired
	}
	if err := ctx.Err(); err != nil {
		return Token{}, err
	}
	if !nonRotating {
		r.RefreshInFlight = true
		if e = m.save(ref, r); e != nil {
			return Token{}, fmt.Errorf("cannot journal credential refresh")
		}
	}
	next, e := d.Refresh(ctx, m.client, t)
	if e != nil || next.AccessToken == "" {
		m.blocked.Store(ref, time.Now().Add(time.Minute))
		if nonRotating {
			if ctx.Err() != nil {
				return Token{}, ctx.Err()
			}
			if errors.Is(e, ErrLoginRequired) {
				return Token{}, ErrLoginRequired
			}
			return Token{}, fmt.Errorf("credential refresh temporarily unavailable; retry later")
		}
		return Token{}, ErrLoginRequired
	}
	if next.RefreshToken == "" {
		next.RefreshToken = t.RefreshToken
	}
	r.Token = &next
	r.ImportedDormant = false
	r.RefreshInFlight = false
	if e = m.save(ref, r); e != nil {
		if nonRotating {
			m.blocked.Store(ref, time.Now().Add(time.Minute))
			return Token{}, fmt.Errorf("refreshed credential could not be persisted; retry later")
		}
		m.blocked.Store(ref, time.Now().Add(time.Hour))
		return Token{}, fmt.Errorf("refreshed credential could not be persisted; sign in again")
	}
	m.blocked.Delete(ref)
	next.BindingID = r.BindingID
	return next, nil
}
func (m *Manager) Start(ctx context.Context) {
	if m.standby {
		return
	}
	go func() {
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				for _, ref := range m.store.Names() {
					if _, _, ok := ParseRef(ref); ok {
						_, _ = m.identity(ctx, ref, "", true)
					}
				}
			}
		}
	}()
}

func (m *Manager) view(driver, account string, r accountRecord) View {
	v := publicView(driver, account, r)
	if d, ok := m.Driver(driver); ok {
		v.BaseURL = d.Info().BaseURL
		v.Protocol = d.Info().Protocol
		if dynamic, ok := d.(EndpointBinding); ok && r.Token != nil {
			if base, protocol, err := dynamic.Binding(*r.Token); err == nil {
				v.BaseURL = base
				v.Protocol = protocol
			}
		}
	}
	return v
}
func (m *Manager) boundInfo(ref string) (Info, error) {
	id, _, ok := ParseRef(ref)
	if !ok {
		return Info{}, ErrLoginRequired
	}
	d, ok := m.Driver(id)
	if !ok {
		return Info{}, ErrLoginRequired
	}
	info := d.Info()
	if dynamic, ok := d.(EndpointBinding); ok {
		if m.store == nil {
			return info, fmt.Errorf("native account needed to resolve endpoint")
		}
		unlock := m.lock(ref)
		defer unlock()
		r, err := m.read(ref)
		if err != nil || r.Token == nil {
			return info, ErrLoginRequired
		}
		base, protocol, err := dynamic.Binding(*r.Token)
		if err != nil {
			return info, err
		}
		info.BaseURL = base
		info.Protocol = protocol
	}
	return info, nil
}
