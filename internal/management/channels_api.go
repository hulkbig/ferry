package management

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"ferry/internal/config"
	"ferry/internal/domain"
	"ferry/internal/egress"
)

type credentialInput struct {
	SecretRef  string `json:"secret_ref"`
	Header     string `json:"header"`
	QueryParam string `json:"query_param"`
	Prefix     string `json:"prefix"`
}

func (in credentialInput) domain() domain.CredentialRef {
	return domain.CredentialRef{
		SecretRef: in.SecretRef, Header: in.Header,
		QueryParam: in.QueryParam, Prefix: in.Prefix,
	}
}

type capabilitiesInput struct {
	Text             bool  `json:"text"`
	Images           bool  `json:"images"`
	Stream           bool  `json:"stream"`
	Tools            bool  `json:"tools"`
	ParallelTools    bool  `json:"parallel_tools"`
	Reasoning        bool  `json:"reasoning"`
	StructuredOutput bool  `json:"structured_output"`
	Vision           bool  `json:"vision"`
	ContextWindow    int64 `json:"context_window"`
	MaxOutput        int64 `json:"max_output"`
}

func (in capabilitiesInput) domain() domain.Capabilities {
	return domain.Capabilities{
		Text: in.Text, Images: in.Images, Stream: in.Stream, Tools: in.Tools,
		ParallelTools: in.ParallelTools, Reasoning: in.Reasoning,
		StructuredOutput: in.StructuredOutput, Vision: in.Vision,
		ContextWindow: in.ContextWindow, MaxOutput: in.MaxOutput,
	}
}

type channelModelInput struct {
	Model        string            `json:"model"`
	Alias        string            `json:"alias"`
	Protocols    []domain.Protocol `json:"protocols"`
	PlanID       string            `json:"plan_id"`
	Capabilities capabilitiesInput `json:"capabilities"`
	Weight       int               `json:"weight"`
	Priority     int               `json:"priority"`
	Pools        []string          `json:"pools"`
}

func (in channelModelInput) domain() domain.ChannelModel {
	return domain.ChannelModel{
		Model: in.Model, Alias: in.Alias, Protocols: in.Protocols,
		PlanID: in.PlanID, Capabilities: in.Capabilities.domain(),
		Weight: in.Weight, Priority: in.Priority, Pools: in.Pools,
	}
}

type channelInput struct {
	ID         string              `json:"id"`
	Type       domain.ChannelType  `json:"type"`
	BaseURL    string              `json:"base_url"`
	Credential credentialInput     `json:"credential"`
	Enabled    *bool               `json:"enabled"`
	Pools      []string            `json:"pools"`
	PlanID     string              `json:"plan_id"`
	Models     []channelModelInput `json:"models"`
}

type channelPatchInput struct {
	Type       *domain.ChannelType  `json:"type"`
	BaseURL    *string              `json:"base_url"`
	Credential *credentialInput     `json:"credential"`
	Enabled    *bool                `json:"enabled"`
	Pools      *[]string            `json:"pools"`
	PlanID     *string              `json:"plan_id"`
	Models     *[]channelModelInput `json:"models"`
}

func (in channelPatchInput) apply(ch *domain.Channel) {
	if in.Type != nil {
		ch.Type = *in.Type
	}
	if in.BaseURL != nil {
		ch.BaseURL = *in.BaseURL
	}
	if in.Credential != nil {
		ch.Credential = in.Credential.domain()
	}
	if in.Enabled != nil {
		v := *in.Enabled
		ch.Enabled = &v
	}
	if in.Pools != nil {
		ch.Pools = append([]string(nil), (*in.Pools)...)
	}
	if in.PlanID != nil {
		ch.PlanID = *in.PlanID
	}
	if in.Models != nil {
		models := make([]domain.ChannelModel, 0, len(*in.Models))
		for _, m := range *in.Models {
			models = append(models, m.domain())
		}
		ch.Models = models
	}
}

func (in channelInput) domain() domain.Channel {
	models := make([]domain.ChannelModel, 0, len(in.Models))
	for _, m := range in.Models {
		models = append(models, m.domain())
	}
	enabled := in.Enabled
	if enabled == nil {
		value := false
		enabled = &value
	}
	return domain.Channel{
		ID: in.ID, Type: in.Type, BaseURL: in.BaseURL,
		Credential: in.Credential.domain(), Enabled: enabled,
		Pools: in.Pools, PlanID: in.PlanID, Models: models,
	}
}

type channelModelView struct {
	Model        string              `json:"model"`
	Alias        string              `json:"alias,omitempty"`
	Protocols    []domain.Protocol   `json:"protocols"`
	PlanID       string              `json:"plan_id,omitempty"`
	Capabilities domain.Capabilities `json:"capabilities"`
	Weight       int                 `json:"weight,omitempty"`
	Priority     int                 `json:"priority,omitempty"`
	Pools        []string            `json:"pools,omitempty"`
	TargetID     string              `json:"target_id"`
	Live         bool                `json:"live"`
}

type channelCredentialView struct {
	SecretRef  string `json:"secret_ref"`
	Header     string `json:"header,omitempty"`
	QueryParam string `json:"query_param,omitempty"`
	Prefix     string `json:"prefix,omitempty"`
}

type channelView struct {
	ID            string                 `json:"id"`
	Type          domain.ChannelType     `json:"type"`
	BaseURL       string                 `json:"base_url,omitempty"`
	CredentialRef string                 `json:"credential_ref,omitempty"`
	Credential    *channelCredentialView `json:"credential,omitempty"`
	Enabled       bool                   `json:"enabled"`
	Pools         []string               `json:"pools,omitempty"`
	PlanID        string                 `json:"plan_id,omitempty"`
	Models        []channelModelView     `json:"models"`
}

func (s *Server) channelView(ch *domain.Channel) channelView {
	v := channelView{
		ID: ch.ID, Type: ch.Type, BaseURL: ch.BaseURL,
		CredentialRef: ch.Credential.SecretRef,
		Enabled:       ch.EnabledOrDefault(), Pools: ch.Pools, PlanID: ch.PlanID,
		Models: make([]channelModelView, 0, len(ch.Models)),
	}
	if ch.Credential.SecretRef != "" {
		v.Credential = &channelCredentialView{
			SecretRef: ch.Credential.SecretRef, Header: ch.Credential.Header,
			QueryParam: ch.Credential.QueryParam, Prefix: ch.Credential.Prefix,
		}
	}
	for i := range ch.Models {
		m := &ch.Models[i]
		tid := domain.ResourceTargetID(domain.ChannelTargetID(ch.ID, m.Model))
		v.Models = append(v.Models, channelModelView{
			Model: m.Model, Alias: m.Alias, Protocols: m.Protocols,
			PlanID: m.PlanID, Capabilities: m.Capabilities,
			Weight: m.Weight, Priority: m.Priority, Pools: m.Pools,
			TargetID: string(tid), Live: s.deps.Scheduler.HasCaller(tid),
		})
	}
	return v
}

func (s *Server) handleChannelsList(w http.ResponseWriter, _ *http.Request) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	items := make([]channelView, 0, len(s.deps.Config.Channels))
	for i := range s.deps.Config.Channels {
		items = append(items, s.channelView(&s.deps.Config.Channels[i]))
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"channels": items})
}

func (s *Server) handleChannelDetail(w http.ResponseWriter, r *http.Request) {
	s.configMu.RLock()
	defer s.configMu.RUnlock()
	id := r.PathValue("id")
	idx := channelIndex(s.deps.Config, id)
	if idx < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown channel "+id)
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{"channel": s.channelView(&s.deps.Config.Channels[idx])})
}

func cloneConfig(f *config.File) (*config.File, error) {
	raw, err := json.Marshal(f)
	if err != nil {
		return nil, fmt.Errorf("encode current config: %w", err)
	}
	var out config.File
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, fmt.Errorf("clone current config: %w", err)
	}
	return &out, nil
}

// publishConfig is the management-plane config transaction boundary. It
// validates and precompiles before invoking the configured SQLite persistence
// and live-publish callback. The caller holds configMu for the full transaction;
// the live config pointer changes only after both gates succeed.
func (s *Server) publishConfig(next *config.File) error {
	if next == nil {
		return fmt.Errorf("config must be non-nil")
	}
	if err := next.Validate(); err != nil {
		return err
	}
	if s.deps.Compiler == nil {
		return fmt.Errorf("compiler not configured")
	}
	if _, err := s.deps.Compiler.Compile(next); err != nil {
		return err
	}
	if s.deps.Publish == nil {
		return fmt.Errorf("publish pipeline not configured")
	}
	if err := s.deps.Publish(next); err != nil {
		return err
	}
	s.prevConfig = s.deps.Config
	s.deps.Config = next
	return nil
}

func (s *Server) handleChannelCreate(w http.ResponseWriter, r *http.Request) {
	var in channelInput
	if err := decodeMgmtBody(r, &in); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	s.configMu.Lock()
	defer s.configMu.Unlock()
	for i := range s.deps.Config.Channels {
		if s.deps.Config.Channels[i].ID == in.ID {
			writeMgmtErr(w, http.StatusConflict, "channel "+in.ID+" already exists")
			return
		}
	}
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	next.Channels = append(next.Channels, in.domain())
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	snap := s.deps.Scheduler.Snapshot()
	writeMgmtJSON(w, http.StatusCreated, map[string]any{
		"published": true, "version": snap.Version,
		"channel": s.channelView(&next.Channels[len(next.Channels)-1]),
	})
}

func channelIndex(f *config.File, id string) int {
	for i := range f.Channels {
		if f.Channels[i].ID == id {
			return i
		}
	}
	return -1
}

func (s *Server) handleChannelPatch(w http.ResponseWriter, r *http.Request) {
	var in channelPatchInput
	if err := decodeMgmtBody(r, &in); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	id := r.PathValue("id")
	s.configMu.Lock()
	defer s.configMu.Unlock()
	idx := channelIndex(s.deps.Config, id)
	if idx < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown channel "+id)
		return
	}
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	in.apply(&next.Channels[idx])
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	snap := s.deps.Scheduler.Snapshot()
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"published": true, "version": snap.Version,
		"channel": s.channelView(&next.Channels[idx]),
	})
}

func (s *Server) handleChannelDelete(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	s.configMu.Lock()
	defer s.configMu.Unlock()
	idx := channelIndex(s.deps.Config, id)
	if idx < 0 {
		writeMgmtErr(w, http.StatusNotFound, "unknown channel "+id)
		return
	}
	next, err := cloneConfig(s.deps.Config)
	if err != nil {
		writeMgmtErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	next.Channels = append(next.Channels[:idx], next.Channels[idx+1:]...)
	if err := s.publishConfig(next); err != nil {
		writeMgmtErr(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	snap := s.deps.Scheduler.Snapshot()
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"deleted": id, "published": true, "version": snap.Version,
	})
}

func findChannelModel(ch *domain.Channel, name string) *domain.ChannelModel {
	if name == "" && len(ch.Models) > 0 {
		return &ch.Models[0]
	}
	for i := range ch.Models {
		if ch.Models[i].Model == name || ch.Models[i].Alias == name {
			return &ch.Models[i]
		}
	}
	return nil
}

// handleChannelTest performs one minimal request directly through the chosen
// egress adapter. It intentionally bypasses Scheduler.Execute, so operator
// connectivity probes never create request/attempt/trace ledger records.
func (s *Server) handleChannelTest(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string          `json:"model"`
		Protocol domain.Protocol `json:"protocol"`
	}
	if err := decodeMgmtBody(r, &body); err != nil {
		writeMgmtErr(w, http.StatusBadRequest, "decode body: "+err.Error())
		return
	}
	id := r.PathValue("id")
	s.configMu.RLock()
	idx := channelIndex(s.deps.Config, id)
	if idx < 0 {
		s.configMu.RUnlock()
		writeMgmtErr(w, http.StatusNotFound, "unknown channel "+id)
		return
	}
	ch := s.deps.Config.Channels[idx]
	s.configMu.RUnlock()
	if !ch.EnabledOrDefault() {
		writeMgmtErr(w, http.StatusConflict, "channel "+id+" is disabled")
		return
	}
	model := findChannelModel(&ch, body.Model)
	if model == nil {
		writeMgmtErr(w, http.StatusBadRequest, "unknown model "+body.Model+" for channel "+id)
		return
	}
	proto := body.Protocol
	if proto == "" {
		proto = domain.PreferredProtocol(model.Protocols)
	}
	if !domain.HasProtocol(model.Protocols, proto) {
		writeMgmtErr(w, http.StatusBadRequest, "protocol "+string(proto)+" is not native for model "+model.Model)
		return
	}

	base := egress.Base{
		HTTP: &http.Client{Timeout: 15 * time.Second},
		Endpoint: &domain.Endpoint{
			ID: "channel-test-" + ch.ID, BaseURL: ch.BaseURL, Protocol: proto,
		},
		CredRef: ch.Credential.SecretRef, Credential: ch.Credential, Model: model.Model,
	}
	var caller egress.Caller
	switch ch.Type {
	case "", domain.ChannelCustom:
		if s.deps.Resolver == nil {
			writeMgmtErr(w, http.StatusServiceUnavailable, "credential resolver not configured")
			return
		}
		secret, err := s.deps.Resolver.Resolve(ch.Credential.SecretRef)
		if err != nil {
			writeMgmtErr(w, http.StatusUnprocessableEntity, "credential unavailable")
			return
		}
		base.Secret = secret
		caller = egress.NewCaller(base, proto)
	case domain.ChannelCliproxyCodex:
		if s.deps.OAuth == nil {
			writeMgmtErr(w, http.StatusServiceUnavailable, "oauth accounts not configured")
			return
		}
		accessToken, accountID, err := s.deps.OAuth.Identity(r.Context(), ch.ID)
		if err != nil {
			writeMgmtErr(w, http.StatusConflict, "channel account is not logged in")
			return
		}
		base.Secret = accessToken
		base.CodexClientVersion = s.currentConfig().EffectiveCodexClientVersion()
		base.AfterResponse = func(headers http.Header) { s.deps.OAuth.ObserveUsageHeaders(ch.ID, accountID, headers) }
		base.BeforeSend = func(ctx context.Context) *domain.Failure { return s.deps.OAuth.CheckUsage(ctx, ch.ID, accountID) }
		if proto == domain.ProtocolOpenAIImages {
			caller = egress.NewCodexImages(base, accountID)
		} else {
			caller = egress.NewCodexCaller(base, accountID)
		}
	default:
		writeMgmtErr(w, http.StatusBadRequest, "unsupported channel type "+string(ch.Type))
		return
	}

	rc := connectionTestContract(proto, model.Model)
	timeout := 15 * time.Second
	if proto == domain.ProtocolOpenAIImages {
		timeout = 120 * time.Second
		base.HTTP.Timeout = timeout
	}
	ctx, cancel := context.WithTimeout(r.Context(), timeout)
	defer cancel()
	started := time.Now()
	resp, failure := caller.Invoke(ctx, rc)
	latency := time.Since(started).Milliseconds()
	if failure != nil {
		writeMgmtJSON(w, http.StatusOK, map[string]any{
			"ok": false, "channel": id, "model": model.Model,
			"protocol": proto, "latency_ms": latency, "failure": failure,
		})
		return
	}
	writeMgmtJSON(w, http.StatusOK, map[string]any{
		"ok": true, "channel": id, "model": model.Model,
		"protocol": proto, "latency_ms": latency,
		"finish_reason": resp.FinishReason,
	})
}
