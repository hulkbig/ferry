package management

import (
	"bytes"
	"mime"
	"net/http"
	"path"
	"strings"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	sched "ferry/internal/scheduler"
	consoleassets "ferry/web/console"
)

// policyPayload is structured JSON emitted by the visual policy editor.
type policyPayload struct {
	Config          *config.File `json:"config,omitempty"`
	ExpectedVersion *int64       `json:"expected_version,omitempty"`
}

// resolveConfig materializes an explicit structured config or the live one.
func (s *Server) resolveConfig(body *policyPayload) (*config.File, error) {
	current := s.currentConfig()
	if body == nil || body.Config == nil {
		// File.Validate resolves references and canonicalizes PolicyGraphs in
		// place. Validate a private copy so the read-only endpoint never mutates
		// the immutable live snapshot after releasing configMu.
		return cloneConfig(current)
	}
	return s.resolveConfigAgainst(body, current)
}

// resolveConfigAgainst merges redacted fields against a caller-selected live
// snapshot. Mutation handlers call it while holding configMu; read-only
// validation passes currentConfig() above.
func (s *Server) resolveConfigAgainst(body *policyPayload, current *config.File) (*config.File, error) {
	if body != nil && body.Config != nil {
		// Broad structured config views redact bearer values. A visual policy or
		// channel edit therefore posts blank keys for unchanged consumers; merge
		// those values from the current in-memory secret boundary before validate.
		existing := map[string]string{}
		if current != nil {
			// The database directory is a process bootstrap setting, not a
			// management-plane field. Preserve it when accepting a structured
			// policy document that omitted or redacted server settings.
			body.Config.Server.DataDir = current.Server.DataDir
			for _, consumer := range current.ConsumerKeys {
				existing[consumer.Name] = consumer.Key
			}
		}
		for i := range body.Config.ConsumerKeys {
			if body.Config.ConsumerKeys[i].Key == "" {
				body.Config.ConsumerKeys[i].Key = existing[body.Config.ConsumerKeys[i].Name]
			}
		}
		return body.Config, nil
	}
	return current, nil
}

// handleValidate compiles a candidate config without publishing (PRD §16.5).
func (s *Server) handleValidate(w http.ResponseWriter, r *http.Request) {
	var body policyPayload
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	var (
		f              *config.File
		controlVersion int64
		err            error
	)
	if body.Config != nil && body.ExpectedVersion != nil {
		if *body.ExpectedVersion <= 0 {
			writeMgmtJSON(w, http.StatusBadRequest, map[string]any{
				"valid": false, "error": "expected_version must be positive",
			})
			return
		}
		s.configMu.RLock()
		controlVersion, err = s.controlVersionLocked()
		if err == nil && controlVersion == *body.ExpectedVersion {
			f, err = s.resolveConfigAgainst(&body, s.deps.Config)
		}
		s.configMu.RUnlock()
		if err != nil {
			writeMgmtJSON(w, http.StatusServiceUnavailable, map[string]any{
				"valid": false, "error": err.Error(),
			})
			return
		}
		if controlVersion != *body.ExpectedVersion {
			writeMgmtJSON(w, http.StatusConflict, map[string]any{
				"valid": false, "error": "structured config is stale",
				"expected_version": *body.ExpectedVersion, "current_version": controlVersion,
			})
			return
		}
	} else {
		f, err = s.resolveConfig(&body)
	}
	if err != nil {
		writeMgmtJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	// Posted configs bypass config.Load, so structural validation (which also
	// resolves target->endpoint/plan references) must run here; compiling an
	// unresolved file would dereference nil pointers.
	if err := f.Validate(); err != nil {
		writeMgmtJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	if _, err := s.deps.Compiler.Compile(f); err != nil {
		writeMgmtJSON(w, http.StatusOK, map[string]any{"valid": false, "error": err.Error()})
		return
	}
	response := map[string]any{"valid": true}
	if controlVersion > 0 {
		response["control_version"] = controlVersion
		writeControlVersionHeaders(w, controlVersion)
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

// handleDryRun plans a request against a candidate or live policy without
// executing it and without touching the ledger (PRD §16.5).
func (s *Server) handleDryRun(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model     string `json:"model"`
		Prompt    string `json:"prompt"`
		Stream    bool   `json:"stream"`
		Objective string `json:"objective"`
		MaxTokens int64  `json:"max_tokens"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	rc := &domain.RequestContract{
		Protocol:  domain.ProtocolOpenAIChat,
		Model:     body.Model,
		MaxTokens: body.MaxTokens,
		Stream:    body.Stream,
		Messages: []domain.Message{{
			Role:   domain.RoleUser,
			Blocks: []domain.Block{{Kind: domain.BlockText, Text: body.Prompt}},
		}},
	}
	meta := sched.RequestMeta{}
	for _, consumer := range s.currentConfig().ConsumerKeys {
		if consumer.Enabled == nil || *consumer.Enabled {
			meta.CallerID = consumer.Name
			break
		}
	}
	if body.Objective != "" {
		// objective override rides the rc.Model alias path; authentication stays
		// bound to an enabled managed consumer for this management-only dry run.
	}
	plan, pol, f := s.deps.Scheduler.DryRun(rc, meta)
	if f != nil {
		excl, _ := s.deps.Scheduler.DryRunExclusions(rc, meta)
		writeMgmtJSON(w, http.StatusOK, map[string]any{
			"ok": false, "failure": f, "exclusions": excl,
		})
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"ok": true, "plan": plan, "policy": pol.Name, "policy_hash": pol.Hash,
	})
}

// handlePublish atomically hot-publishes a new config (PRD §16.6). Compile
// errors are rejected without disturbing in-flight requests.
func (s *Server) handlePublish(w http.ResponseWriter, r *http.Request) {
	var body policyPayload
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	if body.Config != nil && body.ExpectedVersion == nil {
		writeMgmtJSON(w, http.StatusPreconditionRequired, map[string]any{
			"published": false, "error": "expected_version is required for explicit full config publish",
		})
		return
	}
	if body.ExpectedVersion != nil && *body.ExpectedVersion <= 0 {
		writeMgmtJSON(w, http.StatusBadRequest, map[string]any{
			"published": false, "error": "expected_version must be positive",
		})
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if body.ExpectedVersion != nil {
		currentVersion, err := s.controlVersionLocked()
		if err != nil {
			writeMgmtJSON(w, http.StatusServiceUnavailable, map[string]any{
				"published": false, "error": err.Error(),
			})
			return
		}
		if currentVersion != *body.ExpectedVersion {
			writeMgmtJSON(w, http.StatusConflict, map[string]any{
				"published": false, "error": "structured config is stale",
				"expected_version": *body.ExpectedVersion, "current_version": currentVersion,
			})
			return
		}
	}
	var f *config.File
	var err error
	if body.Config == nil {
		// Resolve the implicit candidate only after taking the write lock; an
		// earlier snapshot could otherwise overwrite a concurrent channel edit.
		f = s.deps.Config
	} else {
		f, err = s.resolveConfigAgainst(&body, s.deps.Config)
	}
	if err != nil {
		writeMgmtJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"published": false, "error": err.Error(),
		})
		return
	}
	if err := s.publishConfig(f); err != nil {
		writeMgmtJSON(w, http.StatusUnprocessableEntity, map[string]any{
			"published": false, "error": err.Error(),
		})
		return
	}
	snap := s.deps.Scheduler.Snapshot()
	response := map[string]any{
		"published": true, "version": snap.Version, "hash": snap.Hash,
	}
	if controlVersion, err := s.controlVersionLocked(); err == nil {
		response["control_version"] = controlVersion
		writeControlVersionHeaders(w, controlVersion)
	}
	writeMgmtJSON(w, http.StatusOK, response)
}

// handleRollback restores the previous SQLite-backed control-plane version.
// Tests and lightweight embedders without a persistent callback retain the
// one-step in-memory fallback.
func (s *Server) handleRollback(w http.ResponseWriter, _ *http.Request) {
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if s.deps.Rollback != nil {
		cur := s.deps.Config
		prev, err := s.deps.Rollback()
		if err != nil {
			writeMgmtErr(w, http.StatusConflict, err.Error())
			return
		}
		s.deps.Config = prev
		s.prevConfig = cur
		snap := s.deps.Scheduler.Snapshot()
		writeMgmtJSON(w, http.StatusOK, map[string]any{
			"rolled_back": true, "version": snap.Version,
		})
		return
	}
	prev := s.prevConfig
	if prev == nil {
		writeMgmtErr(w, http.StatusConflict, "no previous version to roll back to")
		return
	}
	if err := s.publishConfig(prev); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	snap := s.deps.Scheduler.Snapshot()
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"rolled_back": true, "version": snap.Version,
	})
}

// handleClearCooldown manually re-opens a breaker/cooldown target.
func (s *Server) handleClearCooldown(w http.ResponseWriter, r *http.Request) {
	id := domain.ResourceTargetID(r.PathValue("id"))
	snap := s.deps.Scheduler.Snapshot()
	if snap.Targets[id] == nil {
		writeMgmtErr(w, http.StatusNotFound, "unknown target "+string(id))
		return
	}
	if err := s.deps.Manager.ResetCooldowns([]domain.ResourceTargetID{id}); err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "runtime reset applied in memory but persistence failed: "+err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"cleared": string(id)})
}

// handleSetQuota overrides locally-known quota for a target.
func (s *Server) handleSetQuota(w http.ResponseWriter, r *http.Request) {
	id := domain.ResourceTargetID(r.PathValue("id"))
	var body struct {
		Used  float64 `json:"used"`
		Limit float64 `json:"limit"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	snap := s.deps.Scheduler.Snapshot()
	if snap.Targets[id] == nil {
		writeMgmtErr(w, http.StatusNotFound, "unknown target "+string(id))
		return
	}
	s.deps.Manager.SetQuota(id, body.Used, body.Limit, time.Now(), "manual", domain.ConfHigh)
	writeMgmtJSON(w, http.StatusOK, map[string]any{"set": string(id)})
}

// handleConsole serves the hash-routed production console and its reviewed static
// assets. The handoff defaults to Mock for standalone demos; the embedded
// entry point switches that default to the same-origin HTTP adapter.
func (s *Server) handleConsole(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		w.Header().Set("Allow", "GET, HEAD")
		writeMgmtErr(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	name := strings.TrimPrefix(r.URL.Path, "/")
	if name == "" {
		name = "index.html"
	}
	if path.Clean(name) != name || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	b, err := consoleassets.Assets.ReadFile(name)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	if name == "index.html" {
		b = bytes.Replace(b, []byte(`data-default-adapter="mock"`), []byte(`data-default-adapter="http"`), 1)
	}
	contentType := mime.TypeByExtension(path.Ext(name))
	if contentType == "" {
		contentType = "text/plain; charset=utf-8"
	}
	w.Header().Set("Content-Type", contentType)
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_, _ = w.Write(b) // #nosec G705 -- b comes from the compile-time embedded console filesystem, never request or upstream input.
}
