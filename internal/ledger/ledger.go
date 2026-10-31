// Package ledger persists logical requests, attempts, and decision traces
// to SQLite. Writes go through a single-writer async queue so the data
// plane never blocks on disk (F-P0-19..22, G5: ledger writes never block
// streaming).
//
// Concurrency model: one dedicated write connection drains the async queue
// (batching adjacent jobs into transactions), while a small pool of read
// connections serves queries. Under WAL, readers never block the writer and
// vice versa — dashboard queries no longer stall attempt inserts.
package ledger

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"

	"ferry/internal/domain"
)

// purgeBatch caps rows deleted per statement inside the retention sweeper so
// one pass never holds the write lock long enough to stall ingestion.
const purgeBatch = 5000

// Ledger is the async write facade + query API.
type Ledger struct {
	db    *sql.DB // dedicated single write connection
	read  *sql.DB // read pool; WAL lets these run beside the writer
	jobs  []writeJob
	ready *sync.Cond
	done  chan struct{}

	retention time.Duration // 0 disables the periodic purge loop
	closeOnce sync.Once
	queueMu   sync.Mutex // protects the non-blocking write backlog
	closed    bool

	mu         sync.Mutex
	requests   map[string]*pendingRequest   // requestID -> accumulating record
	liveWrites map[string]*pendingLiveWrite // coalesced queued snapshots, protected by mu

	debugCapture atomic.Bool
}

type pendingRequest struct {
	rec              domain.RequestRecord
	attempt          domain.AttemptRecord // last attempt (for failure propagation)
	trace            *domain.TraceRecord  // ledger-owned immutable snapshot
	ingress          bool
	awaitingDelivery bool
	finished         bool
	firstOutput      bool
}

// execer abstracts *sql.DB and *sql.Tx so batched writes can run inside one
// transaction without the jobs knowing.
type execer interface {
	Exec(query string, args ...any) (sql.Result, error)
}

type writeJob func(ex execer) error

// Open creates/opens the SQLite database under dir and starts the writer.
// retention > 0 starts a background sweeper deleting rows older than that;
// retention <= 0 keeps history forever.
func Open(dir string, queueSize int, retention time.Duration) (*Ledger, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create data dir: %w", err)
	}
	path := filepath.Join(dir, "ledger.db")
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=synchronous(NORMAL)&_pragma=busy_timeout(5000)&_pragma=auto_vacuum(INCREMENTAL)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1) // dedicated writer connection
	read, err := sql.Open("sqlite", dsn)
	if err != nil {
		db.Close() // #nosec G104 -- error-path cleanup; the original error is returned
		return nil, fmt.Errorf("open sqlite read pool: %w", err)
	}
	read.SetMaxOpenConns(4)
	read.SetMaxIdleConns(4)
	if queueSize <= 0 {
		queueSize = 4096
	}
	l := &Ledger{
		db:        db,
		read:      read,
		jobs:      make([]writeJob, 0, queueSize),
		done:      make(chan struct{}),
		retention: retention,
		requests:  map[string]*pendingRequest{},
	}
	l.ready = sync.NewCond(&l.queueMu)
	if err := l.migrate(); err != nil {
		db.Close()   // #nosec G104 -- error-path cleanup; the original error is returned
		read.Close() // #nosec G104 -- error-path cleanup; the original error is returned
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()   // #nosec G104 -- error-path cleanup; the original error is returned
		read.Close() // #nosec G104 -- error-path cleanup; the original error is returned
		return nil, fmt.Errorf("secure ledger database: %w", err)
	}
	if err := l.openLiveRequests(); err != nil {
		db.Close()
		read.Close()
		return nil, err
	}
	go l.writer()
	if retention > 0 {
		go l.retentionLoop()
	}
	return l, nil
}

func (l *Ledger) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS logical_requests (
			id TEXT PRIMARY KEY,
			ts INTEGER NOT NULL,
			consumer_key TEXT DEFAULT '',
			api_key_secret_id TEXT DEFAULT '',
			alias TEXT DEFAULT '',
			objective TEXT DEFAULT '',
			policy_name TEXT DEFAULT '',
			policy_version INTEGER DEFAULT 0,
			policy_hash TEXT DEFAULT '',
			policy_index_hash TEXT DEFAULT '',
			policy_compile_cache_hit INTEGER DEFAULT 0,
			policy_scene_cache_hit INTEGER DEFAULT 0,
			fallback_count INTEGER DEFAULT 0,
			connection_rebind_count INTEGER DEFAULT 0,
			route_fallback_count INTEGER DEFAULT 0,
			route_advance_count INTEGER DEFAULT 0,
			recovery_fallback_count INTEGER DEFAULT 0,
			conversation_migration_count INTEGER DEFAULT 0,
			model_requested TEXT DEFAULT '',
			model_selected TEXT DEFAULT '',
			protocol TEXT DEFAULT '',
			stream INTEGER DEFAULT 0,
			logical_model_id TEXT DEFAULT '',
			provider_id TEXT DEFAULT '',
			connection_id TEXT DEFAULT '',
			upstream_model TEXT DEFAULT '',
			runtime_revision INTEGER DEFAULT 0,
			route_fingerprint TEXT DEFAULT '',
			commit_at INTEGER DEFAULT 0,
			continuity_mode TEXT DEFAULT '',
			status TEXT DEFAULT '',
			outcome TEXT DEFAULT '',
			failure_type TEXT DEFAULT '',
			failure_msg TEXT DEFAULT '',
			attempts INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			upstream_input_tokens INTEGER DEFAULT 0,
			upstream_output_tokens INTEGER DEFAULT 0,
			upstream_cache_read_tokens INTEGER DEFAULT 0,
			upstream_cache_write_tokens INTEGER DEFAULT 0,
			upstream_reasoning_tokens INTEGER DEFAULT 0,
			est_cash_cost REAL DEFAULT 0,
			quota_units REAL DEFAULT 0,
			allocated_sub_cost REAL DEFAULT 0,
			shadow_cost REAL DEFAULT 0,
			serving_cost REAL DEFAULT 0,
			recovery_overhead_cost REAL DEFAULT 0,
			recovery_added_latency_ms INTEGER DEFAULT 0,
			selected_target TEXT DEFAULT '',
			duration_ms INTEGER DEFAULT 0,
			ttft_ms INTEGER DEFAULT 0,
			compat_level TEXT DEFAULT '',
			compat_notes TEXT DEFAULT '[]',
			exclusions TEXT DEFAULT '[]',
			client_ip TEXT DEFAULT ''
		)`,
		`CREATE INDEX IF NOT EXISTS idx_lr_ts ON logical_requests(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_lr_status ON logical_requests(status, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_lr_alias ON logical_requests(alias, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_lr_target ON logical_requests(selected_target)`,
		`CREATE INDEX IF NOT EXISTS idx_lr_policy ON logical_requests(policy_name, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_lr_failure ON logical_requests(failure_type, ts)`,
		`CREATE TABLE IF NOT EXISTS attempts (
			request_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			ts INTEGER NOT NULL,
			target_id TEXT NOT NULL,
			provider_id TEXT DEFAULT '',
			connection_id TEXT DEFAULT '',
			endpoint_url TEXT DEFAULT '',
			protocol TEXT DEFAULT '',
			credential_ref TEXT DEFAULT '',
			model TEXT DEFAULT '',
			plan_kind TEXT DEFAULT '',
			status TEXT DEFAULT '',
			failure_type TEXT DEFAULT '',
			failure_scope TEXT DEFAULT '',
			failure_status INTEGER DEFAULT 0,
			failure_msg TEXT DEFAULT '',
			latency_ms INTEGER DEFAULT 0,
			ttft_ms INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			cache_write_tokens INTEGER DEFAULT 0,
			reasoning_tokens INTEGER DEFAULT 0,
			multiplier REAL DEFAULT 1,
			est_cash_cost REAL DEFAULT 0,
			quota_units REAL DEFAULT 0,
			allocated_sub_cost REAL DEFAULT 0,
			shadow_cost REAL DEFAULT 0,
			rate_card_version TEXT DEFAULT '',
			quota_rule_version TEXT DEFAULT '',
			multiplier_rule_version TEXT DEFAULT '',
			PRIMARY KEY (request_id, seq)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_att_ts ON attempts(ts)`,
		`CREATE INDEX IF NOT EXISTS idx_att_target ON attempts(target_id, ts)`,
		`CREATE TABLE IF NOT EXISTS decision_traces (
			request_id TEXT PRIMARY KEY,
			ts INTEGER NOT NULL,
			trace TEXT NOT NULL
		)`,
		`CREATE INDEX IF NOT EXISTS idx_dt_ts ON decision_traces(ts)`,
		`CREATE TABLE IF NOT EXISTS request_debug (
			request_id TEXT PRIMARY KEY,
			ts INTEGER NOT NULL,
			request_json TEXT DEFAULT '',
			response_json TEXT DEFAULT '',
			request_truncated INTEGER DEFAULT 0,
			response_truncated INTEGER DEFAULT 0
		)`,
		`CREATE INDEX IF NOT EXISTS idx_request_debug_ts ON request_debug(ts)`,
		`CREATE TABLE IF NOT EXISTS runtime_settings (
			key TEXT PRIMARY KEY,
			value TEXT NOT NULL,
			updated_at INTEGER NOT NULL
		)`,
		// Hourly usage rollup maintained on insert (performance: dashboard
		// aggregates scale with hours×targets, not request count).
		`CREATE TABLE IF NOT EXISTS usage_hourly (
			bucket INTEGER NOT NULL,          -- epoch ms floored to the hour
			target TEXT NOT NULL,
			requests INTEGER DEFAULT 0,
			failures INTEGER DEFAULT 0,
			canceled INTEGER DEFAULT 0,
			input_tokens INTEGER DEFAULT 0,
			output_tokens INTEGER DEFAULT 0,
			cache_read_tokens INTEGER DEFAULT 0,
			est_cash_cost REAL DEFAULT 0,
			quota_units REAL DEFAULT 0,
			latency_sum_ms INTEGER DEFAULT 0, -- sums duration_ms where > 0
			latency_count INTEGER DEFAULT 0,
			latency_max_ms INTEGER DEFAULT 0,
			PRIMARY KEY (bucket, target)
		)`,
	}
	for _, s := range stmts {
		if _, err := l.db.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{"policy_hash", "TEXT DEFAULT ''"},
		{"api_key_secret_id", "TEXT DEFAULT ''"},
		{"policy_index_hash", "TEXT DEFAULT ''"},
		{"policy_compile_cache_hit", "INTEGER DEFAULT 0"},
		{"policy_scene_cache_hit", "INTEGER DEFAULT 0"},
		{"fallback_count", "INTEGER DEFAULT 0"},
		{"connection_rebind_count", "INTEGER DEFAULT 0"},
		{"route_fallback_count", "INTEGER DEFAULT 0"},
		{"route_advance_count", "INTEGER DEFAULT 0"},
		{"recovery_fallback_count", "INTEGER DEFAULT 0"},
		{"conversation_migration_count", "INTEGER DEFAULT 0"},
		{"outcome", "TEXT DEFAULT ''"},
		{"logical_model_id", "TEXT DEFAULT ''"},
		{"provider_id", "TEXT DEFAULT ''"},
		{"connection_id", "TEXT DEFAULT ''"},
		{"upstream_model", "TEXT DEFAULT ''"},
		{"service_tier", "TEXT DEFAULT ''"},
		{"response_model", "TEXT DEFAULT ''"},
		{"response_service_tier", "TEXT DEFAULT ''"},
		{"service_tier_evidence", "TEXT DEFAULT ''"},
		{"cache_subset_of_input", "INTEGER"},
		{"upstream_cache_subset_of_input", "INTEGER"},
		{"runtime_revision", "INTEGER DEFAULT 0"},
		{"route_fingerprint", "TEXT DEFAULT ''"},
		{"commit_at", "INTEGER DEFAULT 0"},
		{"continuity_mode", "TEXT DEFAULT ''"},
		{"upstream_input_tokens", "INTEGER DEFAULT 0"},
		{"upstream_output_tokens", "INTEGER DEFAULT 0"},
		{"upstream_cache_read_tokens", "INTEGER DEFAULT 0"},
		{"upstream_cache_write_tokens", "INTEGER DEFAULT 0"},
		{"upstream_reasoning_tokens", "INTEGER DEFAULT 0"},
		{"serving_cost", "REAL DEFAULT 0"},
		{"recovery_overhead_cost", "REAL DEFAULT 0"},
		{"recovery_added_latency_ms", "INTEGER DEFAULT 0"},
	} {
		if err := addColumnIfMissing(l.db, "logical_requests", column.name, column.definition); err != nil {
			return err
		}
	}
	for _, column := range []string{"service_tier", "response_model", "response_service_tier", "service_tier_evidence"} {
		if err := addColumnIfMissing(l.db, "attempts", column, "TEXT DEFAULT ''"); err != nil {
			return err
		}
	}
	if err := addColumnIfMissing(l.db, "attempts", "cache_subset_of_input", "INTEGER"); err != nil {
		return err
	}
	if err := addColumnIfMissing(l.db, "attempts", "failure_scope", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(l.db, "attempts", "provider_id", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	if err := addColumnIfMissing(l.db, "attempts", "connection_id", "TEXT DEFAULT ''"); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE INDEX IF NOT EXISTS idx_lr_logical_model ON logical_requests(logical_model_id, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_lr_provider ON logical_requests(provider_id, ts)`,
		`CREATE INDEX IF NOT EXISTS idx_lr_outcome ON logical_requests(outcome, ts)`,
	} {
		if _, err := l.db.Exec(statement); err != nil {
			return err
		}
	}
	var debugValue string
	if err := l.db.QueryRow(`SELECT value FROM runtime_settings WHERE key = 'debug_request_response'`).Scan(&debugValue); err != nil && err != sql.ErrNoRows {
		return fmt.Errorf("load runtime settings: %w", err)
	}
	l.debugCapture.Store(debugValue == "true")
	return nil
}

func addColumnIfMissing(db *sql.DB, table, column, definition string) error {
	rows, err := db.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return err
	}
	found := false
	for rows.Next() {
		var cid, notNull, pk int
		var name, typ string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &typ, &notNull, &defaultValue, &pk); err != nil {
			rows.Close() // #nosec G104 -- error-path cleanup; the original error is returned
			return err
		}
		if name == column {
			found = true
		}
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if found {
		return nil
	}
	if _, err := db.Exec("ALTER TABLE " + table + " ADD COLUMN " + column + " " + definition); err != nil {
		return fmt.Errorf("migrate %s.%s: %w", table, column, err)
	}
	return nil
}

// writer drains the queue serially. Adjacent backlog is drained (up to
// maxBatch jobs) and committed as one transaction — bursts cost one commit
// instead of one per row. Any failed job rolls the whole batch back and the
// jobs are replayed one-by-one so a single bad row cannot drop its siblings.
func (l *Ledger) writer() {
	defer close(l.done)
	const maxBatch = 128
	for {
		l.queueMu.Lock()
		for len(l.jobs) == 0 && !l.closed {
			l.ready.Wait()
		}
		if len(l.jobs) == 0 && l.closed {
			l.queueMu.Unlock()
			return
		}
		n := min(len(l.jobs), maxBatch)
		batch := append([]writeJob(nil), l.jobs[:n]...)
		clear(l.jobs[:n])
		l.jobs = l.jobs[n:]
		l.queueMu.Unlock()
		tx, err := l.db.Begin()
		if err != nil {
			l.replay(batch)
			continue
		}
		failed := false
		for _, j := range batch {
			if err := j(tx); err != nil {
				failed = true
				break
			}
		}
		if failed {
			_ = tx.Rollback()
			l.replay(batch)
			continue
		}
		if err := tx.Commit(); err != nil {
			l.replay(batch)
		}
	}
}

// replay runs jobs individually, reporting each failure, so one poison job
// never takes out a batch.
func (l *Ledger) replay(batch []writeJob) {
	for _, j := range batch {
		if err := j(l.db); err != nil {
			fmt.Fprintf(os.Stderr, "ledger write: %v\n", err)
		}
	}
}

func (l *Ledger) enqueue(job writeJob) {
	l.queueMu.Lock()
	defer l.queueMu.Unlock()
	if l.closed {
		return
	}
	// Keep writes in order without blocking streaming on SQLite or dropping
	// lifecycle records when a traffic burst exceeds the initial capacity.
	l.jobs = append(l.jobs, job)
	l.ready.Signal()
}

// retentionLoop sweeps expired rows once an hour until Close.
func (l *Ledger) retentionLoop() {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		select {
		case <-t.C:
			cutoff := time.Now().Add(-l.retention)
			if _, err := l.PurgeOlderThan(cutoff); err != nil {
				fmt.Fprintf(os.Stderr, "ledger retention: %v\n", err)
			}
		case <-l.done:
			return
		}
	}
}

// PurgeOlderThan deletes logical requests, attempts, and traces whose ts is
// before cutoff, in bounded batches so no single statement holds the write
// lock long. Returns the number of logical requests removed.
func (l *Ledger) PurgeOlderThan(cutoff time.Time) (int64, error) {
	ms := cutoff.UnixMilli()
	// Children first: purging attempts/traces before their parent request
	// keeps every intermediate state consistent with the FK-less schema.
	purge := func(query string) (int64, error) {
		var total int64
		for {
			res, err := l.db.Exec(query, ms, purgeBatch)
			if err != nil {
				return total, err
			}
			n, err := res.RowsAffected()
			if err != nil {
				return total, err
			}
			total += n
			if n < purgeBatch {
				return total, nil
			}
		}
	}
	if _, err := purge(`DELETE FROM attempts WHERE rowid IN
		(SELECT rowid FROM attempts WHERE ts < ? LIMIT ?)`); err != nil {
		return 0, fmt.Errorf("purge attempts: %w", err)
	}
	if _, err := purge(`DELETE FROM decision_traces WHERE request_id IN
		(SELECT request_id FROM decision_traces WHERE ts < ? LIMIT ?)`); err != nil {
		return 0, fmt.Errorf("purge traces: %w", err)
	}
	if _, err := purge(`DELETE FROM request_debug WHERE request_id IN
		(SELECT request_id FROM request_debug WHERE ts < ? LIMIT ?)`); err != nil {
		return 0, fmt.Errorf("purge request debug: %w", err)
	}
	removed, err := purge(`DELETE FROM logical_requests WHERE id IN
		(SELECT id FROM logical_requests WHERE ts < ? LIMIT ?)`)
	if err != nil {
		return removed, fmt.Errorf("purge requests: %w", err)
	}
	// Reclaim freed pages incrementally (bounded, unlike full VACUUM).
	_, _ = l.db.Exec(`PRAGMA incremental_vacuum(1024)`)
	return removed, nil
}

// BeginRequest registers a new logical request.
func (l *Ledger) BeginRequest(rec domain.RequestRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.requests[rec.ID] = &pendingRequest{rec: rec}
	l.enqueue(func(db execer) error { return insertRequestRow(db, &rec, "live_requests") })
}

// DebugCaptureEnabled reports the persisted request/response debug setting.
func (l *Ledger) DebugCaptureEnabled() bool { return l != nil && l.debugCapture.Load() }

// SetDebugCapture persists and immediately applies the debug capture setting.
func (l *Ledger) SetDebugCapture(enabled bool) error {
	if l == nil {
		return fmt.Errorf("ledger is not configured")
	}
	value := "false"
	if enabled {
		value = "true"
	}
	if _, err := l.db.Exec(`INSERT INTO runtime_settings (key, value, updated_at) VALUES ('debug_request_response', ?, ?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, updated_at = excluded.updated_at`, value, time.Now().UnixMilli()); err != nil {
		return fmt.Errorf("persist debug setting: %w", err)
	}
	l.debugCapture.Store(enabled)
	return nil
}

// CaptureDebugRequest records a redacted request for requests that begin
// while capture is enabled. Writes remain asynchronous like the main ledger.
func (l *Ledger) CaptureDebugRequest(requestID string, ts time.Time, payload string, truncated bool) {
	if l == nil || !l.debugCapture.Load() {
		return
	}
	l.enqueue(func(db execer) error {
		_, err := db.Exec(`INSERT OR REPLACE INTO request_debug
			(request_id, ts, request_json, response_json, request_truncated, response_truncated)
			VALUES (?, ?, ?, '', ?, 0)`, requestID, ts.UnixMilli(), payload, b2i(truncated))
		return err
	})
}

// CaptureDebugResponse completes an existing debug row. If capture was not
// active when the request began, the update intentionally affects no rows.
func (l *Ledger) CaptureDebugResponse(requestID, payload string, truncated bool) {
	if l == nil {
		return
	}
	l.enqueue(func(db execer) error {
		_, err := db.Exec(`UPDATE request_debug SET response_json = ?, response_truncated = ? WHERE request_id = ?`, payload, b2i(truncated), requestID)
		return err
	})
}

// AddAttempt records one attempt against its logical request. The attempt row
// is written immediately; the parent request row is finalized on Finish.
func (l *Ledger) AddAttempt(a domain.AttemptRecord) {
	l.mu.Lock()
	p, ok := l.requests[a.RequestID]
	if ok {
		p.attempt = a
	}
	l.mu.Unlock()
	l.enqueue(func(db execer) error { return insertAttempt(db, &a) })
}

// SetTrace publishes an owned snapshot of the decision trace. The caller must
// keep tr and its descendants stable during this call, but may reuse or mutate
// them after it returns. Readers and the async writer never share caller data.
func (l *Ledger) SetTrace(requestID string, tr *domain.TraceRecord) {
	snapshot := cloneTraceRecord(tr)
	l.mu.Lock()
	defer l.mu.Unlock()
	if p, ok := l.requests[requestID]; ok {
		p.trace = snapshot
	}
}

// cloneTraceRecord copies every mutable descendant. Keep this in sync with
// domain.TraceRecord when adding slices, maps, or pointers to the trace types.
func cloneTraceRecord(tr *domain.TraceRecord) *domain.TraceRecord {
	if tr == nil {
		return nil
	}
	copy := *tr
	copy.Tiers = slices.Clone(tr.Tiers)
	copy.Candidates = slices.Clone(tr.Candidates)
	for i := range copy.Candidates {
		copy.Candidates[i].Scores = maps.Clone(tr.Candidates[i].Scores)
	}
	copy.ExecutionOrder = slices.Clone(tr.ExecutionOrder)
	copy.RecoveryEvents = slices.Clone(tr.RecoveryEvents)
	copy.AffinityEvidence = slices.Clone(tr.AffinityEvidence)
	if tr.AffinityBinding != nil {
		binding := *tr.AffinityBinding
		copy.AffinityBinding = &binding
	}
	copy.Events = slices.Clone(tr.Events)
	return &copy
}

// FinishRequest marks execution complete. Observed ingress requests remain
// live until the downstream handler returns; other requests finalize here.
func (l *Ledger) FinishRequest(id string, mutate func(r *domain.RequestRecord)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p, ok := l.requests[id]
	if !ok || p.finished {
		return
	}
	clientTTFT := p.rec.TTFTMS
	if mutate != nil {
		mutate(&p.rec)
	}
	p.finished = true
	if p.ingress || p.firstOutput {
		// Attempt completion must not replace the downstream delivery clock.
		p.rec.TTFTMS = clientTTFT
	}
	if p.awaitingDelivery {
		l.persistLiveRequest(p)
		return
	}
	l.finishRequestLocked(id, p)
}

func (l *Ledger) finishRequestLocked(id string, p *pendingRequest) {
	rec := p.rec
	trace := p.trace
	delete(l.requests, id)

	l.enqueue(func(db execer) error {
		if err := insertRequest(db, &rec); err != nil {
			return err
		}
		_, err := db.Exec("DELETE FROM live_requests WHERE id = ?", rec.ID)
		return err
	})
	if trace != nil {
		tr := *trace
		// SetTrace owns this immutable snapshot, including its descendants.
		// Trace ts mirrors the request ts so retention ages traces by when
		// the request happened, not when the async writer flushed.
		l.enqueue(func(db execer) error { return insertTrace(db, id, rec.Ts, &tr) })
	}
}

// Close flushes pending writes and closes the databases. Idempotent.
func (l *Ledger) Close() error {
	var err error
	l.closeOnce.Do(func() {
		l.queueMu.Lock()
		l.closed = true
		l.ready.Broadcast()
		l.queueMu.Unlock()
		<-l.done
		err = l.db.Close()
		if rerr := l.read.Close(); err == nil {
			err = rerr
		}
	})
	return err
}

// ---------------------------------------------------------------------------
// inserts

func insertAttempt(db execer, a *domain.AttemptRecord) error {
	evidence, _ := json.Marshal(a.ServiceTierEvidence)
	failType, failScope, failStatus, failMsg := "", "", 0, ""
	if a.Failure != nil {
		failType = string(a.Failure.Type)
		failScope = string(a.Failure.EffectiveScope())
		failStatus = a.Failure.HTTPStatus
		failMsg = a.Failure.Message
	}
	_, err := db.Exec(`INSERT OR REPLACE INTO attempts (
		request_id, seq, ts, target_id, provider_id, connection_id, endpoint_url, protocol, credential_ref,
		model, plan_kind, status, failure_type, failure_scope, failure_status, failure_msg,
		latency_ms, ttft_ms, input_tokens, output_tokens, cache_read_tokens,
		cache_write_tokens, reasoning_tokens, multiplier, est_cash_cost,
		quota_units, allocated_sub_cost, shadow_cost, rate_card_version,
		quota_rule_version, multiplier_rule_version, service_tier, response_model, response_service_tier, cache_subset_of_input, service_tier_evidence
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		a.RequestID, a.Seq, a.Ts.UnixMilli(), string(a.TargetID), a.ProviderID, a.ConnectionID, a.EndpointURL,
		string(a.Protocol), a.CredentialRef, a.Model, string(a.PlanKind),
		string(a.Status), failType, failScope, failStatus, failMsg,
		a.LatencyMS, a.TTFTMS, a.Usage.InputTokens, a.Usage.OutputTokens,
		a.Usage.CacheReadTokens, a.Usage.CacheWriteTokens, a.Usage.ReasoningTokens,
		a.Multiplier, a.EstCashCost, a.QuotaUnits, a.AllocatedSubCost,
		a.ShadowCost, a.RateCardVersion, a.QuotaRuleVersion, a.MultiplierRuleVersion,
		a.ServiceTier, a.ResponseModel, a.ResponseServiceTier, b2i(a.Usage.CacheSubsetOfInput), string(evidence))
	return err
}

func insertRequest(db execer, r *domain.RequestRecord) error {
	if err := insertRequestRow(db, r, "logical_requests"); err != nil {
		return err
	}
	return upsertRollup(db, r)
}

func insertRequestRow(db execer, r *domain.RequestRecord, table string) error {
	evidence, _ := json.Marshal(r.ServiceTierEvidence)
	notes, _ := json.Marshal(orEmptySliceStr(r.CompatNotes))
	exc, _ := json.Marshal(orEmptySliceExc(r.Exclusions))
	columns := `id, ts, consumer_key, api_key_secret_id, alias, objective, policy_name, policy_version,
		policy_hash, policy_index_hash, policy_compile_cache_hit, policy_scene_cache_hit, fallback_count,
		connection_rebind_count, route_fallback_count, route_advance_count, recovery_fallback_count,
		conversation_migration_count, model_requested, model_selected, protocol, stream,
		logical_model_id, provider_id, connection_id, upstream_model, runtime_revision, route_fingerprint, commit_at, continuity_mode,
		status, outcome, failure_type, failure_msg, attempts,
		input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
		upstream_input_tokens, upstream_output_tokens, upstream_cache_read_tokens,
		upstream_cache_write_tokens, upstream_reasoning_tokens,
		est_cash_cost, quota_units, allocated_sub_cost, shadow_cost, serving_cost,
		recovery_overhead_cost, recovery_added_latency_ms, selected_target, duration_ms, ttft_ms,
		compat_level, compat_notes, exclusions, client_ip, service_tier, response_model, response_service_tier,
		cache_subset_of_input, upstream_cache_subset_of_input, service_tier_evidence`
	commitAt := int64(0)
	if !r.CommitAt.IsZero() {
		commitAt = r.CommitAt.UnixMilli()
	}
	args := []any{
		r.ID, r.Ts.UnixMilli(), r.ConsumerKey, r.APIKeySecretID, r.Alias, r.Objective,
		r.PolicyName, r.PolicyVersion,
		r.PolicyHash, r.PolicyIndexHash, b2i(r.PolicyCompileCacheHit), b2i(r.PolicySceneCacheHit), r.FallbackCount,
		r.ConnectionRebindCount, r.RouteFallbackCount, r.RouteAdvanceCount, r.RecoveryFallbackCount,
		r.ConversationMigrationCount, r.ModelRequested, r.ModelSelected,
		string(r.Protocol), b2i(r.Stream), r.LogicalModelID, r.ProviderID, r.ConnectionID, r.UpstreamModel,
		r.RuntimeRevision, r.RouteFingerprint, commitAt, string(r.ContinuityMode),
		string(r.Status), string(r.Outcome), string(r.FailureType), r.FailureMsg, r.Attempts,
		r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheReadTokens,
		r.Usage.CacheWriteTokens, r.Usage.ReasoningTokens,
		r.UpstreamUsage.InputTokens, r.UpstreamUsage.OutputTokens, r.UpstreamUsage.CacheReadTokens,
		r.UpstreamUsage.CacheWriteTokens, r.UpstreamUsage.ReasoningTokens,
		r.EstCashCost, r.QuotaUnits, r.AllocatedSubCost, r.ShadowCost,
		r.ServingCost, r.RecoveryOverheadCost, r.RecoveryAddedLatencyMS,
		string(r.SelectedTarget), r.DurationMS, r.TTFTMS,
		string(r.CompatLevel), string(notes), string(exc), r.ClientIP,
		r.ServiceTier, r.ResponseModel, r.ResponseServiceTier, b2i(r.Usage.CacheSubsetOfInput), b2i(r.UpstreamUsage.CacheSubsetOfInput),
		string(evidence),
	}
	query := `INSERT OR REPLACE INTO ` + table + ` (` + columns + `) VALUES (` +
		strings.TrimSuffix(strings.Repeat("?,", len(args)), ",") + `)`
	_, err := db.Exec(query, args...)
	if err != nil {
		return err
	}
	return nil
}

// upsertRollup folds one finalized logical request into its hour bucket.
// Called only alongside insertRequest; single-writer ordering makes the
// pair atomic enough without an explicit transaction.
func upsertRollup(db execer, r *domain.RequestRecord) error {
	bucket := r.Ts.UnixMilli() / 3600000
	failure, canceled := 0, 0
	switch r.Status {
	case domain.ReqSuccess:
	case domain.ReqCanceled:
		canceled = 1
	default:
		failure = 1 // failed and any other non-success terminal state
	}
	latSum, latCnt := int64(0), int64(0)
	if r.DurationMS > 0 {
		latSum, latCnt = r.DurationMS, 1
	}
	_, err := db.Exec(`INSERT INTO usage_hourly (
		bucket, target, requests, failures, canceled, input_tokens, output_tokens,
		cache_read_tokens, est_cash_cost, quota_units,
		latency_sum_ms, latency_count, latency_max_ms
	) VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?)
	ON CONFLICT(bucket, target) DO UPDATE SET
		requests = requests + excluded.requests,
		failures = failures + excluded.failures,
		canceled = canceled + excluded.canceled,
		input_tokens = input_tokens + excluded.input_tokens,
		output_tokens = output_tokens + excluded.output_tokens,
		cache_read_tokens = cache_read_tokens + excluded.cache_read_tokens,
		est_cash_cost = est_cash_cost + excluded.est_cash_cost,
		quota_units = quota_units + excluded.quota_units,
		latency_sum_ms = latency_sum_ms + excluded.latency_sum_ms,
		latency_count = latency_count + excluded.latency_count,
		latency_max_ms = MAX(latency_max_ms, excluded.latency_max_ms)`,
		bucket, string(r.SelectedTarget), 1, failure, canceled,
		r.Usage.InputTokens, r.Usage.OutputTokens, r.Usage.CacheReadTokens,
		r.EstCashCost, r.QuotaUnits, latSum, latCnt, r.DurationMS)
	return err
}

func insertTrace(db execer, requestID string, ts time.Time, tr *domain.TraceRecord) error {
	b, err := json.Marshal(tr)
	if err != nil {
		return err
	}
	_, err = db.Exec(`INSERT OR REPLACE INTO decision_traces (request_id, ts, trace) VALUES (?,?,?)`,
		requestID, ts.UnixMilli(), string(b))
	return err
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

func orEmptySliceStr(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

func orEmptySliceExc(s []domain.ExclusionRecord) []domain.ExclusionRecord {
	if s == nil {
		return []domain.ExclusionRecord{}
	}
	return s
}
