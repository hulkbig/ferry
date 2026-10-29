package management

import (
	"crypto/rand"
	"encoding/base64"
	"net/http"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/ledger"
)

func maskConsumerKey(key string) string {
	if len(key) <= 7 {
		return "••••"
	}
	return key[:3] + "••••••••" + key[len(key)-4:]
}

func generateConsumerKey() (string, error) {
	raw := make([]byte, 24)
	if _, err := rand.Read(raw); err != nil {
		return "", err
	}
	return "sk-ferry-" + base64.RawURLEncoding.EncodeToString(raw), nil
}

func findConsumerIndex(f *config.File, name string) int {
	for i := range f.ConsumerKeys {
		if f.ConsumerKeys[i].Name == name {
			return i
		}
	}
	return -1
}

func (s *Server) handleConsumerKeyReveal(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	cfg := s.currentConfig()
	i := findConsumerIndex(cfg, name)
	if i < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown consumer "+name)
		return
	}
	writeMgmtJSON(w, http.StatusGone, map[string]any{
		"error": "API key secrets are only returned once at creation or rotation",
		"name":  name,
	})
}

func (s *Server) handleConsumerCreate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Name             string  `json:"name"`
		DefaultAlias     string  `json:"default_alias"`
		DefaultObjective string  `json:"default_objective"`
		MonthlyBudgetUSD float64 `json:"monthly_budget_usd"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	if findConsumerIndex(s.deps.Config, body.Name) >= 0 {
		writeMgmtErr(w, http.StatusConflict, "consumer already exists")
		return
	}
	key, err := generateConsumerKey()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, "generate key: "+err.Error())
		return
	}
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	enabled := true
	next.ConsumerKeys = append(next.ConsumerKeys, config.ConsumerKey{
		Name: body.Name, Key: key, Enabled: &enabled,
		DefaultAlias: body.DefaultAlias, DefaultObjective: body.DefaultObjective,
		MonthlyBudgetUSD: body.MonthlyBudgetUSD,
	})
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusCreated, map[string]any{
		"created": body.Name, "key": key, "key_preview": maskConsumerKey(key),
	})
}

func (s *Server) handleConsumerRotate(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.configMu.Lock()
	defer s.configMu.Unlock()
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	i := findConsumerIndex(next, name)
	if i < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown consumer "+name)
		return
	}
	key, err := generateConsumerKey()
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	next.ConsumerKeys[i].Key = key
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"rotated": name, "key": key, "key_preview": maskConsumerKey(key)})
}

func (s *Server) handleConsumerUpdate(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Enabled          *bool    `json:"enabled"`
		DefaultAlias     *string  `json:"default_alias"`
		DefaultObjective *string  `json:"default_objective"`
		MonthlyBudgetUSD *float64 `json:"monthly_budget_usd"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	i := findConsumerIndex(next, r.PathValue("name"))
	if i < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown consumer "+r.PathValue("name"))
		return
	}
	if body.Enabled != nil {
		next.ConsumerKeys[i].Enabled = body.Enabled
	}
	if body.DefaultAlias != nil {
		next.ConsumerKeys[i].DefaultAlias = *body.DefaultAlias
	}
	if body.DefaultObjective != nil {
		next.ConsumerKeys[i].DefaultObjective = *body.DefaultObjective
	}
	if body.MonthlyBudgetUSD != nil {
		next.ConsumerKeys[i].MonthlyBudgetUSD = *body.MonthlyBudgetUSD
	}
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"updated": r.PathValue("name")})
}

func (s *Server) handleConsumerDelete(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	s.configMu.Lock()
	defer s.configMu.Unlock()
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	i := findConsumerIndex(next, name)
	if i < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown consumer "+name)
		return
	}
	if len(next.ConsumerKeys) == 1 {
		writeMgmtErr(w, http.StatusConflict, "cannot delete the last consumer key")
		return
	}
	next.ConsumerKeys = append(next.ConsumerKeys[:i], next.ConsumerKeys[i+1:]...)
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"deleted": name})
}

func (s *Server) handleConsumerAnalytics(w http.ResponseWriter, r *http.Request) {
	name := r.PathValue("name")
	to := time.Now()
	from := to.Add(-24 * time.Hour)
	if h, err := time.ParseDuration(r.URL.Query().Get("hours") + "h"); err == nil && h > 0 {
		from = to.Add(-h)
	}
	all, err := s.deps.Ledger.ByConsumer(from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	summary := all[name]
	if summary == nil {
		summary = &ledger.UsageSummary{}
	}
	byTarget, err := s.deps.Ledger.ByConsumerDimension(name, "selected_target", from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	byModel, err := s.deps.Ledger.ByConsumerDimension(name, "model_selected", from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	failures, err := s.deps.Ledger.ConsumerFailureBreakdown(name, from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	fallbacks, err := s.deps.Ledger.ConsumerFallbackCount(name, from, to)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	recent, _, err := s.deps.Ledger.QueryRequests(domain.LogFilter{ConsumerKey: name, From: from, To: to, Limit: 8})
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"name": name, "from": from, "to": to, "summary": summary,
		"fallback_requests": fallbacks, "by_target": byTarget,
		"by_model": byModel, "failures": failures, "recent_requests": recent,
	})
}
