package runtime

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"time"

	"ferry/internal/domain"
)

// Cooldown persistence.
//
// Throttle and quota state is the record of what the upstream has already
// told us. Losing it on restart means a connection that was parked until its
// plan resets is picked again immediately and hits the same wall — the worst
// possible traffic pattern to present to a provider that is already unhappy
// with us. The state is therefore snapshotted to disk and reloaded at start,
// with anything already expired discarded on the way in.
const cooldownStateFile = "cooldowns.json"

type connectionCooldown struct {
	Identity     string                    `json:"identity,omitempty"`
	CoolingUntil time.Time                 `json:"cooling_until,omitempty"`
	Throttled    bool                      `json:"throttled,omitempty"`
	QuotaWindows []domain.QuotaWindowState `json:"quota_windows,omitempty"`
}

type targetCooldown struct {
	QuotaCooldownUntil time.Time `json:"quota_cooldown_until,omitempty"`
	Identity           string    `json:"identity,omitempty"`
	CooldownUntil      time.Time `json:"cooldown_until,omitempty"`
	QuotaResetAt       time.Time `json:"quota_reset_at,omitempty"`
}

type cooldownSnapshot struct {
	Version     int                           `json:"version"`
	SavedAt     time.Time                     `json:"saved_at"`
	Connections map[string]connectionCooldown `json:"connections,omitempty"`
	Targets     map[string]targetCooldown     `json:"targets,omitempty"`
}

// StartCooldownPersistence loads any previously saved cooldown state and keeps
// it written back for the lifetime of ctx. Persistence is best effort: a
// failure to read or write is logged and never blocks request handling.
func (m *Manager) StartCooldownPersistence(ctx context.Context, dataDir string) {
	if dataDir == "" {
		return
	}
	path := filepath.Join(dataDir, cooldownStateFile)
	m.mu.Lock()
	m.cooldownPath = path
	if m.cooldownDirty == nil {
		m.cooldownDirty = make(chan struct{}, 1)
	}
	dirty := m.cooldownDirty
	m.mu.Unlock()

	if err := m.loadCooldowns(path); err != nil && !os.IsNotExist(err) {
		log.Printf("runtime: cooldown state not restored: %v", err)
	}
	go func() {
		// Coalesce bursts: one write per interval is enough to survive a
		// crash, and cooldown deadlines are seconds-to-hours scale anyway.
		ticker := time.NewTicker(5 * time.Second)
		defer ticker.Stop()
		pending := false
		for {
			select {
			case <-ctx.Done():
				if err := m.saveCooldowns(); err != nil {
					log.Printf("runtime: final cooldown save failed: %v", err)
				}
				return
			case <-dirty:
				pending = true
			case <-ticker.C:
				if !pending {
					continue
				}
				pending = false
				if err := m.saveCooldowns(); err != nil {
					log.Printf("runtime: cooldown save failed: %v", err)
				}
			}
		}
	}()
}

// markCooldownDirtyLocked signals the writer without blocking. Callers hold
// m.mu; the send never blocks because the channel is buffered and coalescing.
func (m *Manager) markCooldownDirtyLocked() {
	if m.cooldownDirty == nil {
		return
	}
	select {
	case m.cooldownDirty <- struct{}{}:
	default:
	}
}

func (m *Manager) saveCooldowns() error {
	// Serialize snapshot creation as well as rename: an older periodic save
	// must never overwrite a reset which already returned successfully.
	m.cooldownSaveMu.Lock()
	defer m.cooldownSaveMu.Unlock()
	m.mu.Lock()
	path := m.cooldownPath
	if path == "" {
		m.mu.Unlock()
		return nil
	}
	now := time.Now()
	snapshot := cooldownSnapshot{
		Version:     2,
		SavedAt:     now,
		Connections: map[string]connectionCooldown{},
		Targets:     map[string]targetCooldown{},
	}
	for id, connection := range m.connections {
		entry := connectionCooldown{
			Identity:     m.identities[id],
			CoolingUntil: connection.st.CoolingUntil,
			Throttled:    connection.adm.throttled,
			QuotaWindows: activeQuotaWindows(connection.st.QuotaWindows, now),
		}
		if entry.CoolingUntil.After(now) || entry.Throttled || len(entry.QuotaWindows) > 0 {
			snapshot.Connections[id] = entry
		}
	}
	for id, target := range m.targets {
		entry := targetCooldown{
			QuotaCooldownUntil: target.quotaCooldownUntil,
			Identity:           m.identities[m.targetConnections[id]],
			CooldownUntil:      target.st.CooldownUntil,
			QuotaResetAt:       target.st.QuotaResetAt,
		}
		if entry.CooldownUntil.After(now) || entry.QuotaResetAt.After(now) {
			snapshot.Targets[string(id)] = entry
		}
	}
	m.mu.Unlock()

	raw, err := json.Marshal(snapshot)
	if err != nil {
		return err
	}
	// Write-and-rename so a crash mid-write cannot leave a truncated file that
	// would be discarded on the next start.
	temp := path + ".tmp"
	if err := os.WriteFile(temp, raw, 0o600); err != nil {
		return fmt.Errorf("write cooldown state: %w", err)
	}
	if err := os.Rename(temp, path); err != nil {
		return fmt.Errorf("commit cooldown state: %w", err)
	}
	return nil
}

func (m *Manager) loadCooldowns(path string) error {
	raw, err := os.ReadFile(path) // #nosec G304 -- path is the data-dir cooldown state file previously written by this process via write-and-rename.
	if err != nil {
		return err
	}
	var snapshot cooldownSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		return fmt.Errorf("decode cooldown state: %w", err)
	}
	// Old files contain only reusable names, with no evidence of ownership.
	// Do not attach those observations to potentially replaced accounts.
	if snapshot.Version != 2 {
		log.Printf("runtime: discarded cooldown format %d without supported ownership metadata", snapshot.Version)
		return nil
	}
	now := time.Now()
	m.mu.Lock()
	defer m.mu.Unlock()
	for id, entry := range snapshot.Connections {
		if m.managed && (m.connections[id] == nil || entry.Identity == "" || entry.Identity != m.identities[id]) {
			continue
		}
		windows := activeQuotaWindows(entry.QuotaWindows, now)
		if !entry.CoolingUntil.After(now) && len(windows) == 0 && !entry.Throttled {
			continue
		}
		connection := m.connections[id]
		if connection == nil {
			connection = &connectionRuntime{st: domain.ConnectionState{
				ConnectionID: id, Enabled: true,
				Auth:         domain.ConnectionAuthUnknown,
				Reachability: domain.ConnectionReachabilityUnknown,
			}}
			m.connections[id] = connection
		}
		if entry.CoolingUntil.After(now) {
			connection.st.CoolingUntil = entry.CoolingUntil
		}
		connection.st.QuotaWindows = windows
		// A connection that was throttled when we stopped stays serialized
		// until it proves itself, exactly as it would have without a restart.
		connection.adm.throttled = entry.Throttled || len(windows) > 0
	}
	for rawID, entry := range snapshot.Targets {
		id := domain.ResourceTargetID(rawID)
		if m.managed && (m.targets[id] == nil || entry.Identity == "" || entry.Identity != m.identities[m.targetConnections[id]]) {
			continue
		}
		if !entry.CooldownUntil.After(now) && !entry.QuotaResetAt.After(now) {
			continue
		}
		target := m.targets[id]
		if target == nil {
			target = newTargetRuntime()
			m.targets[id] = target
		}
		if entry.CooldownUntil.After(now) {
			target.st.CooldownUntil = entry.CooldownUntil
			target.quotaCooldownUntil = entry.QuotaCooldownUntil
		}
		if entry.QuotaResetAt.After(now) {
			target.st.QuotaResetAt = entry.QuotaResetAt
		}
	}
	return nil
}

// activeQuotaWindows drops windows whose reset has already passed so a long
// downtime does not resurrect an expired exhaustion.
func activeQuotaWindows(windows []domain.QuotaWindowState, now time.Time) []domain.QuotaWindowState {
	out := make([]domain.QuotaWindowState, 0, len(windows))
	for _, window := range windows {
		if window.Availability == domain.QuotaExhausted && window.ResetAt.After(now) {
			out = append(out, window)
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}
