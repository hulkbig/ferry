package ledger

import (
	"ferry/internal/domain"
	"strings"
	"time"
)

// Keep open requests outside finalized billing and analytics cohorts.
// Prefer the final row during handover; a request ID appears exactly once.
const liveRequestsCTE = "WITH logical_requests AS (SELECT * FROM main.logical_requests UNION ALL SELECT * FROM live_requests WHERE id NOT IN (SELECT id FROM main.logical_requests)) "

func (l *Ledger) openLiveRequests() error {
	var schema string
	if err := l.db.QueryRow("SELECT sql FROM sqlite_master WHERE name='logical_requests'").Scan(&schema); err != nil {
		return err
	}
	schema = strings.Replace(schema, "logical_requests", "IF NOT EXISTS live_requests", 1)
	if _, err := l.db.Exec(schema); err != nil {
		return err
	}
	// HTTP clients cannot resume after a restart. Finalize stranded rows once.
	rows, err := l.read.Query("SELECT " + requestCols + " FROM live_requests AS logical_requests WHERE id NOT IN (SELECT id FROM main.logical_requests)")
	if err != nil {
		return err
	}
	var stranded []domain.RequestRecord
	for rows.Next() {
		record, err := scanRequest(rows)
		if err != nil {
			rows.Close()
			return err
		}
		stranded = append(stranded, *record)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	tx, err := l.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, record := range stranded {
		record.Status = domain.ReqCanceled
		record.Outcome = domain.OutcomeFailed
		record.FailureType = domain.FailClientCancel
		record.FailureMsg = "runtime restarted before the request completed"
		record.DurationMS = time.Since(record.Ts).Milliseconds()
		if err := insertRequest(tx, &record); err != nil {
			return err
		}
	}
	if _, err := tx.Exec("DELETE FROM live_requests"); err != nil {
		return err
	}
	return tx.Commit()
}

func (l *Ledger) UpdateRequest(id string, mutate func(*domain.RequestRecord)) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if pending := l.requests[id]; pending != nil {
		mutate(&pending.rec)
		l.persistLiveRequest(pending)
	}
}

// BeginIngressRequest keeps the logical request open until both the scheduler
// and the downstream HTTP handler have completed.
func (l *Ledger) BeginIngressRequest(rec domain.RequestRecord) {
	l.mu.Lock()
	defer l.mu.Unlock()
	p := &pendingRequest{rec: rec, ingress: true, awaitingDelivery: true}
	l.requests[rec.ID] = p
	l.persistLiveRequest(p)
}

func (l *Ledger) EndIngressRequest(id string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.requests[id]; p != nil {
		p.awaitingDelivery = false
		p.rec.DurationMS = time.Since(p.rec.Ts).Milliseconds()
		if p.finished {
			l.finishRequestLocked(id, p)
		}
	}
}

// RecordFirstOutput uses the client ingress clock, independently of per-attempt
// TTFT. Call only after meaningful output has been written to the downstream.
func (l *Ledger) RecordFirstOutput(id string, at time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if p := l.requests[id]; p != nil && !p.firstOutput {
		p.firstOutput = true
		p.rec.TTFTMS = max(1, at.Sub(p.rec.Ts).Milliseconds())
		l.persistLiveRequest(p)
	}
}

func liveRequestRecord(p *pendingRequest) domain.RequestRecord {
	record := p.rec
	if p.finished && p.awaitingDelivery {
		record.Status = "delivering"
		if record.Stream {
			record.Status = "streaming"
		}
		record.Outcome = ""
	}
	record.DurationMS = time.Since(record.Ts).Milliseconds()
	return record
}

// persistLiveRequest is called under mu so live updates cannot overtake the
// final write and resurrect a completed row.
func (l *Ledger) persistLiveRequest(p *pendingRequest) {
	record := liveRequestRecord(p)
	l.enqueue(func(db execer) error { return insertRequestRow(db, &record, "live_requests") })
}

// LiveRequests returns copies from one lock-protected snapshot, including
// ingress decoding and downstream delivery with no active upstream attempt.
func (l *Ledger) LiveRequests() []domain.RequestRecord {
	l.mu.Lock()
	defer l.mu.Unlock()
	rows := make([]domain.RequestRecord, 0, len(l.requests))
	for _, p := range l.requests {
		rows = append(rows, liveRequestRecord(p))
	}
	return rows
}
