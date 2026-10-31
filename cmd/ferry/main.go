// Command ferry runs the Ferry gateway protocols on the data
// plane, the management API + console on a separate local port (PRD §11).
package main

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"ferry/internal/adminauth"
	"ferry/internal/buildinfo"
	"ferry/internal/config"
	"ferry/internal/controlstore"
	"ferry/internal/conversation"
	"ferry/internal/domain"
	"ferry/internal/egress"
	"ferry/internal/ingress"
	"ferry/internal/ledger"
	"ferry/internal/maintenance"
	"ferry/internal/management"
	"ferry/internal/nativeauth"
	"ferry/internal/oauth"
	"ferry/internal/policy"
	"ferry/internal/runtime"
	sched "ferry/internal/scheduler"
	"ferry/internal/secretstore"
)

const serverReadHeaderTimeout = 10 * time.Second

// Catalog-only HTTP clients: the provider models endpoint and the community
// capability aggregate are read-only GETs with bounded latency and no
// cross-origin redirect follow.
const (
	providerModelsClientTimeout = 15 * time.Second
	modelsDevClientTimeout      = 20 * time.Second
	// Reasoning and media-analysis requests can legitimately spend more than
	// five minutes before an upstream emits response headers. Keep this aligned
	// with the longest supported inference clients while still bounding a
	// permanently silent upstream.
	upstreamResponseHeaderTimeout = 2 * time.Hour
	// A client that gives up before response headers arrive leaves no useful
	// evidence that the pooled connection is still healthy. Rotate only that
	// Connection's transport after a meaningful wait so a wedged HTTP/2 session
	// cannot poison later attempts or a provider fallback.
	preHeaderTransportResetAfter = 15 * time.Second
)

type inferenceRoundTripper struct {
	connectionID    string
	responseTimeout time.Duration
	bodyIdleTimeout time.Duration
	resetAfter      time.Duration

	mu         sync.RWMutex
	base       *http.Transport
	generation uint64
}

func (t *inferenceRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	started := time.Now()
	t.mu.RLock()
	base := t.base
	t.mu.RUnlock()
	resp, err := base.RoundTrip(req)
	if err != nil {
		if reason := preHeaderTransportResetReason(req.Context(), err, time.Since(started), t.resetAfter); reason != "" {
			t.rotate(base, reason, time.Since(started))
		}
	}
	if err == nil && resp.Body != nil && t.bodyIdleTimeout > 0 {
		resp.Body = &idleTimeoutBody{
			ReadCloser: resp.Body, timeout: t.bodyIdleTimeout,
			onFailure: func(reason string) { t.rotate(base, reason, time.Since(started)) },
		}
	}
	return resp, err
}

func (t *inferenceRoundTripper) CloseIdleConnections() {
	t.mu.RLock()
	base := t.base
	t.mu.RUnlock()
	base.CloseIdleConnections()
}

func (t *inferenceRoundTripper) rotate(failed *http.Transport, reason string, elapsed time.Duration) {
	t.mu.Lock()
	if t.base != failed {
		t.mu.Unlock()
		return
	}
	t.base = newInferenceTransport(t.responseTimeout)
	t.generation++
	generation := t.generation
	t.mu.Unlock()
	failed.CloseIdleConnections()
	log.Printf("inference transport reset connection=%s generation=%d reason=%s elapsed=%s",
		t.connectionID, generation, reason, elapsed.Round(time.Millisecond))
}

func (t *inferenceRoundTripper) Generation() uint64 {
	t.mu.RLock()
	defer t.mu.RUnlock()
	return t.generation
}

func preHeaderTransportResetReason(ctx context.Context, err error, elapsed, resetAfter time.Duration) string {
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "unexpected_eof"
	}
	if ctx != nil && ctx.Err() != nil {
		if elapsed >= resetAfter {
			return "pre_header_context_end"
		}
		return ""
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return "response_header_timeout"
	}
	return "round_trip_error"
}

type idleTimeoutBody struct {
	io.ReadCloser
	timeout time.Duration

	mu          sync.Mutex
	generation  uint64
	closed      bool
	timedOut    uint64
	onFailure   func(reason string)
	failureOnce sync.Once
}

func (b *idleTimeoutBody) Read(p []byte) (int, error) {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return 0, io.ErrClosedPipe
	}
	b.generation++
	generation := b.generation
	timer := time.AfterFunc(b.timeout, func() {
		b.mu.Lock()
		if b.closed || b.generation != generation {
			b.mu.Unlock()
			return
		}
		b.closed = true
		b.timedOut = generation
		body := b.ReadCloser
		b.mu.Unlock()
		_ = body.Close()
	})
	b.mu.Unlock()

	n, err := b.ReadCloser.Read(p)
	b.mu.Lock()
	_ = timer.Stop()
	timedOut := b.timedOut == generation
	b.generation++ // invalidate a timer callback racing with the completed read
	b.mu.Unlock()
	if timedOut {
		b.reportFailure("response_body_idle_timeout")
		return n, fmt.Errorf("upstream response body idle for %s: %w", b.timeout, context.DeadlineExceeded)
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		b.reportFailure("response_body_unexpected_eof")
	}
	return n, err
}

func (b *idleTimeoutBody) reportFailure(reason string) {
	if b.onFailure != nil {
		b.failureOnce.Do(func() { b.onFailure(reason) })
	}
}

func (b *idleTimeoutBody) Close() error {
	b.mu.Lock()
	b.closed = true
	b.generation++
	body := b.ReadCloser
	b.mu.Unlock()
	return body.Close()
}

func newInferenceTransport(responseTimeout time.Duration) *http.Transport {
	transport := http.DefaultTransport.(*http.Transport).Clone()
	transport.ResponseHeaderTimeout = responseTimeout
	return transport
}

func newConnectionInferenceHTTPClient(connectionID string, responseTimeout, resetAfter time.Duration) *http.Client {
	// http.Client.Timeout also covers response body reads, so using it here
	// cuts off healthy streaming responses after a fixed wall-clock duration.
	// Bound the connection setup through the standard transport and the wait
	// for response headers here. Once streaming starts, only a full idle window
	// or cancellation of the request context ends the response.
	return &http.Client{Transport: &inferenceRoundTripper{
		connectionID: connectionID, responseTimeout: responseTimeout,
		bodyIdleTimeout: responseTimeout, resetAfter: resetAfter,
		base: newInferenceTransport(responseTimeout), generation: 1,
	}}
}

func newInferenceHTTPClient(responseTimeout time.Duration) *http.Client {
	return newConnectionInferenceHTTPClient("test", responseTimeout, preHeaderTransportResetAfter)
}

type inferenceHTTPClientRegistry struct {
	mu              sync.Mutex
	clients         map[string]*http.Client
	responseTimeout time.Duration
	resetAfter      time.Duration
}

func newInferenceHTTPClientRegistry(responseTimeout, resetAfter time.Duration) *inferenceHTTPClientRegistry {
	return &inferenceHTTPClientRegistry{
		clients: map[string]*http.Client{}, responseTimeout: responseTimeout, resetAfter: resetAfter,
	}
}

func (r *inferenceHTTPClientRegistry) Client(connectionID string) *http.Client {
	r.mu.Lock()
	defer r.mu.Unlock()
	if client := r.clients[connectionID]; client != nil {
		return client
	}
	client := newConnectionInferenceHTTPClient(connectionID, r.responseTimeout, r.resetAfter)
	r.clients[connectionID] = client
	return client
}

func (r *inferenceHTTPClientRegistry) CloseIdleConnections() {
	r.mu.Lock()
	clients := make([]*http.Client, 0, len(r.clients))
	for _, client := range r.clients {
		clients = append(clients, client)
	}
	r.mu.Unlock()
	for _, client := range clients {
		client.CloseIdleConnections()
	}
}

func catalogOnlyHTTPClient(timeout time.Duration) *http.Client {
	return &http.Client{
		Timeout: timeout,
		CheckRedirect: func(next *http.Request, via []*http.Request) error {
			if len(via) > 0 && (!strings.EqualFold(next.URL.Host, via[0].URL.Host) ||
				!strings.EqualFold(next.URL.Scheme, via[0].URL.Scheme)) {
				return fmt.Errorf("model catalog redirected to a different origin")
			}
			if len(via) >= 10 {
				return fmt.Errorf("too many model catalog redirects")
			}
			return nil
		},
	}
}

func main() {
	dataDir := flag.String("data-dir", "./data", "SQLite data directory")
	cfgPath := flag.String("config", "", "one-time legacy config import used only when SQLite is empty")
	bootstrapDir := flag.String("bootstrap-dir", "", "private one-time startup config and upstream-key staging directory")
	healthcheckURL := flag.String("healthcheck-url", "", "probe URL and exit (container healthcheck)")
	versionJSON := flag.Bool("version-json", false, "print immutable build identity as JSON and exit")
	migrateOnly := flag.Bool("migrate-only", false, "initialize/validate SQLite and exit without serving")
	startupCheck := flag.Bool("startup-check", false, "print listener addresses without creating or migrating SQLite, then exit")
	headless := flag.Bool("headless", headlessBuild, "serve only inference; disable management HTTP and optional background jobs, retain credential refresh")
	standby := flag.Bool("standby", false, "serve a migration copy without renewing upstream credentials, spending reset credits, or starting upstream background jobs")
	backupTo := flag.String("backup-to", "", "create and verify a configuration-only SQLite backup, then exit")
	backupHistory := flag.Bool("backup-history", false, "include full history in an explicit -backup-to operation")
	verifyBackup := flag.String("verify-backup", "", "verify a SQLite backup directory, then exit")
	restoreFrom := flag.String("restore-from", "", "verify and restore a SQLite backup directory, then exit")
	resetAdminPassword := flag.Bool("reset-admin-password", false, "read and reset the local administrator password from stdin, then exit")
	adminUsername := flag.String("admin-username", "", "administrator username guard for -reset-admin-password")
	flag.Parse()
	if headlessBuild && !*headless {
		log.Fatal("headless build cannot enable management HTTP")
	}

	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	if *versionJSON {
		if err := json.NewEncoder(os.Stdout).Encode(buildinfo.Current()); err != nil {
			log.Fatalf("version: %v", err)
		}
		return
	}
	if *healthcheckURL != "" {
		resp, err := (&http.Client{Timeout: 4 * time.Second}).Get(*healthcheckURL)
		if err != nil {
			log.Fatalf("healthcheck: %v", err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			log.Fatalf("healthcheck: HTTP %d", resp.StatusCode)
		}
		return
	}
	if *backupHistory && *backupTo == "" {
		log.Fatalf("-backup-history requires -backup-to")
	}
	maintenanceActions := 0
	for _, active := range []bool{*startupCheck, *migrateOnly, *backupTo != "", *verifyBackup != "", *restoreFrom != "", *resetAdminPassword} {
		if active {
			maintenanceActions++
		}
	}
	if maintenanceActions > 1 {
		log.Fatalf("maintenance modes are mutually exclusive")
	}
	if maintenanceActions == 1 {
		var result any
		var err error
		switch {
		case *startupCheck:
			startup, startupErr := readStartupConfig(*dataDir, *cfgPath)
			if *headless {
				startup.ManagementAddr = ""
			}
			result, err = startup, startupErr
		case *migrateOnly:
			result, err = maintenance.Migrate(*dataDir, *cfgPath)
		case *backupTo != "":
			if *backupHistory {
				result, err = maintenance.Backup(*dataDir, *backupTo)
			} else {
				result, err = maintenance.BackupConfiguration(*dataDir, *backupTo)
			}
		case *verifyBackup != "":
			result, err = maintenance.Verify(*verifyBackup)
		case *restoreFrom != "":
			err = maintenance.Restore(*restoreFrom, *dataDir)
			result = map[string]bool{"restored": err == nil}
		case *resetAdminPassword:
			result, err = resetAdministratorPassword(*dataDir, *adminUsername, os.Stdin, os.Stderr)
		}
		if err != nil {
			log.Fatalf("maintenance: %v", err)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			log.Fatalf("maintenance result: %v", err)
		}
		return
	}

	if err := os.MkdirAll(*dataDir, 0o700); err != nil {
		log.Fatalf("data dir: %v", err)
	}
	bootstrap, err := openStartupBootstrap(*bootstrapDir)
	if err != nil {
		log.Fatalf("startup bootstrap: %v", err)
	}
	defer bootstrap.close()
	controlPath := filepath.Join(*dataDir, "control.db")
	control, err := controlstore.Open(controlPath)
	if err != nil {
		log.Fatalf("control store: %v", err)
	}
	defer control.Close()

	f, controlVersion, err := control.ActiveConfig()
	if errors.Is(err, controlstore.ErrNoConfig) {
		if *cfgPath == "" {
			f = bootstrap.config
			if f == nil {
				log.Fatalf("control store is empty: provide -config <path> or -bootstrap-dir containing config.yaml")
			}
		} else {
			f, err = config.Load(*cfgPath)
			if err != nil {
				log.Fatalf("legacy config import: %v", err)
			}
		}
		f.Server.DataDir = *dataDir
		controlVersion, err = control.SaveConfig(f)
		if err != nil {
			log.Fatalf("persist imported config: %v", err)
		}
		log.Printf("imported legacy config into SQLite control version=%d", controlVersion)
	} else if err != nil {
		log.Fatalf("load active config: %v", err)
	}
	// The database location is a process-level bootstrap choice. Runtime
	// configuration is read from SQLite and cannot redirect its own database.
	// Validate every decoded version instead of trusting persisted derived
	// Endpoint/Plan pointers or an older PolicyGraphs canonicalization.
	if err := prepareLoadedConfig(f, *dataDir); err != nil {
		log.Fatalf("validate active control config: %v", err)
	}
	for _, violation := range config.OAuthBindingViolations(f) {
		log.Printf("WARNING legacy configuration tolerated at startup: %s; move the connection to its own oauth account before the next publish", violation)
	}
	tz, err := time.LoadLocation(f.Server.Timezone)
	if err != nil {
		log.Fatalf("timezone %q: %v", f.Server.Timezone, err)
	}

	led, err := ledger.Open(f.Server.DataDir, 4096,
		time.Duration(f.Server.LogRetentionDays)*24*time.Hour)
	if err != nil {
		log.Fatalf("ledger: %v", err)
	}
	defer led.Close()

	mgr := runtime.NewManager(runtime.DefaultHealthConfig())
	comp := policy.NewCompilerWithCapabilityEvidence(func(providerID, endpointFingerprint, upstreamModel string,
		protocol domain.Protocol, capability string, now time.Time) (bool, bool, error) {
		evidence, err := control.ProviderCapabilityEvidence(
			providerID, endpointFingerprint, upstreamModel, protocol, capability, now)
		if err != nil {
			return false, false, err
		}
		if evidence == nil {
			return false, false, nil
		}
		return evidence.Supported, true, nil
	})
	sch := sched.New(mgr, led)
	conversationStore, err := conversation.Open(f.Server.DataDir)
	if err != nil {
		log.Fatalf("conversation store: %v", err)
	}
	defer conversationStore.Close()
	sch.SetConversationStore(conversationStore)
	sch.SetAPIKeyUsageRecorder(control.RecordAPIKeySecretUse)

	// Local secret store shares the 0600 SQLite control database. Values are
	// reachable only through explicit secret-management operations.
	store, err := secretstore.OpenSQLite(controlPath)
	if err != nil {
		log.Fatalf("secret store: %v", err)
	}
	defer store.Close()
	if err := bootstrap.seed(controlPath); err != nil {
		log.Fatalf("startup bootstrap: %v", err)
	}
	nativeMgr := nativeauth.DefaultManagerWithOptions(store, nativeauth.Options{Standby: *standby})
	resolver := &config.Resolver{Store: store.Get, NativeOAuth: nativeMgr}
	oauthMgr := oauth.NewManagerWithOptions(store, oauth.Options{Standby: *standby})
	inferenceClients := newInferenceHTTPClientRegistry(
		upstreamResponseHeaderTimeout, preHeaderTransportResetAfter)
	defer inferenceClients.CloseIdleConnections()

	// Publish the startup world at the active durable Configuration Revision.
	// Secret materialization happens
	// here and only here; downstream layers hold resolved values in memory.
	// A target whose key is not yet provisioned does not block startup: it
	// stays in the snapshot but excluded until its key is set via the console.
	apiKeyAuthenticators, err := control.APIKeyAuthenticators()
	if err != nil {
		log.Fatalf("load API key identities: %v", err)
	}
	if err := publish(sch, comp, f, resolver, oauthMgr, inferenceClients.Client,
		apiKeyAuthenticators, controlVersion); err != nil {
		log.Fatalf("compile: %v", err)
	}
	log.Printf("published policy snapshot runtime_revision=%d control_version=%d targets=%d channel-models=%d policies=%d",
		controlVersion,
		controlVersion, len(f.Targets)+channelModelCount(f), channelModelCount(f), len(f.Policies))

	eng := ingress.NewServer(sch)
	mux := http.NewServeMux()
	eng.Register(mux)

	publishAndPersist := func(nf *config.File) error {
		// Treat published config snapshots as immutable. Secret/OAuth refreshes
		// can republish the live pointer under a read lock, so normalize the
		// process-owned data directory on a private copy instead of mutating it.
		raw, err := json.Marshal(nf)
		if err != nil {
			return fmt.Errorf("clone publish config: %w", err)
		}
		var candidate config.File
		if err := json.Unmarshal(raw, &candidate); err != nil {
			return fmt.Errorf("clone publish config: %w", err)
		}
		candidate.Server.DataDir = *dataDir
		if err := candidate.Validate(); err != nil {
			return err
		}
		// Compile before committing the new active row so invalid policy never
		// becomes the durable source of truth.
		if _, err := comp.Compile(&candidate); err != nil {
			return err
		}
		_, activeVersion, err := control.ActiveConfig()
		if err != nil {
			return err
		}
		persistedVersion, err := control.SaveConfig(&candidate)
		if err != nil {
			return err
		}
		authenticators, authErr := control.APIKeyAuthenticators()
		if authErr != nil {
			return fmt.Errorf("load API key identities: %w", authErr)
		}
		if err := publish(sch, comp, &candidate, resolver, oauthMgr, inferenceClients.Client,
			authenticators, persistedVersion); err != nil {
			if persistedVersion != activeVersion {
				if restoreErr := control.RestoreConfig(activeVersion, persistedVersion); restoreErr != nil {
					return fmt.Errorf("publish: %v; restore control version %d: %w", err, activeVersion, restoreErr)
				}
			}
			return err
		}
		// SaveConfig assigns immutable policy revision numbers/content hashes.
		// Reflect that metadata into the management candidate that becomes the
		// process-owned live pointer after this callback returns.
		nf.RoutingPolicies = candidate.RoutingPolicies
		for i := range nf.ConsumerKeys {
			nf.ConsumerKeys[i].Key = ""
		}
		return nil
	}
	rollbackPersisted := func() (*config.File, error) {
		_, currentVersion, err := control.ActiveConfig()
		if err != nil {
			return nil, err
		}
		prev, rollbackVersion, err := control.RollbackConfig()
		if err != nil {
			return nil, err
		}
		if err := prepareLoadedConfig(prev, *dataDir); err != nil {
			if restoreErr := control.ActivateConfig(currentVersion); restoreErr != nil {
				return nil, fmt.Errorf("validate rollback config: %v; reactivate control version %d: %w", err, currentVersion, restoreErr)
			}
			return nil, fmt.Errorf("validate rollback config: %w", err)
		}
		authenticators, authErr := control.APIKeyAuthenticators()
		if authErr != nil {
			return nil, fmt.Errorf("load API key identities: %w", authErr)
		}
		if err := publish(sch, comp, prev, resolver, oauthMgr, inferenceClients.Client,
			authenticators, rollbackVersion); err != nil {
			if restoreErr := control.ActivateConfig(currentVersion); restoreErr != nil {
				return nil, fmt.Errorf("publish rollback: %v; reactivate control version %d: %w", err, currentVersion, restoreErr)
			}
			return nil, err
		}
		return prev, nil
	}
	reloadAPIKeys := func() error {
		authenticators, err := control.APIKeyAuthenticators()
		if err != nil {
			return fmt.Errorf("load API key identities: %w", err)
		}
		return sch.ReplaceAPIKeys(authenticators)
	}
	refreshRuntime := func(active *config.File) error {
		if active == nil {
			return fmt.Errorf("active config unavailable")
		}
		authenticators, err := control.APIKeyAuthenticators()
		if err != nil {
			return fmt.Errorf("load API key identities: %w", err)
		}
		_, revision, err := control.ActiveConfig()
		if err != nil {
			return fmt.Errorf("read active control revision: %w", err)
		}
		return publish(sch, comp, active, resolver, oauthMgr, inferenceClients.Client,
			authenticators, revision)
	}

	mgmt := management.New(management.Deps{
		Scheduler:      sch,
		Manager:        mgr,
		Ledger:         led,
		Config:         f,
		Compiler:       comp,
		Resolver:       resolver,
		Store:          store,
		APIKeys:        control,
		Control:        control,
		Security:       control,
		Publish:        publishAndPersist,
		RefreshRuntime: refreshRuntime,
		ReloadAPIKeys:  reloadAPIKeys,
		ControlVersion: func() (int64, error) {
			_, version, err := control.ActiveConfig()
			return version, err
		},
		Rollback:                rollbackPersisted,
		PolicyHistory:           control,
		Timezone:                tz,
		OAuth:                   oauthMgr,
		NativeOAuth:             nativeMgr,
		InferenceHTTPClient:     inferenceClients.Client,
		ProviderModelHTTPClient: catalogOnlyHTTPClient(providerModelsClientTimeout),
		ModelsDevHTTPClient:     catalogOnlyHTTPClient(modelsDevClientTimeout),
	})
	sch.SetCapabilityVerifier(mgmt.VerifyRequiredCapabilities)
	// NewManager resumes persisted device logins immediately. Reconcile once
	// after the runtime callback is registered so a completion racing startup
	// cannot leave its configured caller disabled until the next config change.
	if err := refreshRuntime(f); err != nil {
		log.Fatalf("reconcile OAuth runtime: %v", err)
	}
	if err := bootstrap.cleanup(*cfgPath); err != nil {
		log.Fatalf("startup bootstrap cleanup: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	// Restore before background probes can publish fresh observations.
	mgr.StartCooldownPersistence(ctx, f.Server.DataDir)
	if !*headless {
		mgmt.StartAlertMonitor(ctx)
	}
	if *standby {
		log.Printf("standby mode: upstream account changes and background jobs disabled; inference still consumes upstream quota")
	} else {
		if !*headless {
			sch.StartScheduledProviderMessages(ctx)
			mgmt.StartCodexClientUpdate(ctx)
			mgmt.StartProviderModelSync(ctx)
			mgmt.StartAutomaticUsageReset(ctx)
			mgmt.StartReferencePricingSync(ctx)
		}
		oauthMgr.StartAutoRefresh(ctx)
		nativeMgr.Start(ctx)
	}

	dataSrv := newHTTPServer(f.Server.ProxyAddr, mux)
	var mgmtSrv *http.Server
	if !*headless {
		mgmtSrv = newHTTPServer(f.Server.ManagementAddr, mgmt.Routes())
	} else {
		log.Printf("headless mode: management HTTP and optional background jobs disabled; credential refresh retained unless standby")
	}

	go func() {
		log.Printf("data plane listening on %s (chat/responses/messages/images)", f.Server.ProxyAddr)
		if err := dataSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatalf("data plane: %v", err)
		}
	}()
	if mgmtSrv != nil {
		go func() {
			log.Printf("management listening on %s", f.Server.ManagementAddr)
			if err := mgmtSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				log.Printf("management: %v", err)
			}
		}()
	}

	<-ctx.Done()
	log.Printf("shutting down...")
	shCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if mgmtSrv != nil {
		_ = mgmtSrv.Shutdown(shCtx)
	}
	oauthCloseCtx, oauthCloseCancel := context.WithTimeout(context.Background(), 5*time.Second)
	_ = oauthMgr.Close(oauthCloseCtx)
	oauthCloseCancel()
	_ = dataSrv.Shutdown(shCtx)
}

func newHTTPServer(addr string, handler http.Handler) *http.Server {
	// Bound slow request headers without imposing a whole-request read or write
	// deadline on long-running and streaming agent requests.
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: serverReadHeaderTimeout,
	}
}

func prepareLoadedConfig(f *config.File, dataDir string) error {
	if f == nil {
		return fmt.Errorf("config must be non-nil")
	}
	f.Server.DataDir = dataDir
	return f.ValidateStartup()
}

func resetAdministratorPassword(dataDir, expectedUsername string, input io.Reader, prompts io.Writer) (map[string]any, error) {
	if err := os.MkdirAll(dataDir, 0o700); err != nil {
		return nil, fmt.Errorf("data dir: %w", err)
	}
	control, err := controlstore.Open(filepath.Join(dataDir, "control.db"))
	if err != nil {
		return nil, fmt.Errorf("control store: %w", err)
	}
	defer control.Close()
	admin, err := control.ManagementAdministrator()
	if err != nil {
		return nil, err
	}
	if admin == nil {
		return nil, controlstore.ErrAdministratorNotInitialized
	}
	if expectedUsername != "" && expectedUsername != admin.Username {
		return nil, fmt.Errorf("administrator username does not match")
	}
	reader := bufio.NewReader(input)
	readPassword := func(prompt string) (string, error) {
		if prompts != nil {
			_, _ = fmt.Fprint(prompts, prompt)
		}
		value, readErr := reader.ReadString('\n')
		if readErr != nil && !errors.Is(readErr, io.EOF) {
			return "", readErr
		}
		value = strings.TrimSuffix(strings.TrimSuffix(value, "\n"), "\r")
		return value, nil
	}
	password, err := readPassword("New administrator password: ")
	if err != nil {
		return nil, err
	}
	confirmation, err := readPassword("Confirm administrator password: ")
	if err != nil {
		return nil, err
	}
	if password != confirmation {
		return nil, fmt.Errorf("password confirmation does not match")
	}
	passwordHash, err := adminauth.HashPassword(password)
	if err != nil {
		return nil, err
	}
	if err := control.UpdateManagementAdministratorPassword(admin.Username, passwordHash,
		"password_reset_local", "local-cli", time.Now()); err != nil {
		return nil, err
	}
	return map[string]any{"reset": true, "username": admin.Username, "sessions_revoked": true}, nil
}

// publish compiles a config file into a live snapshot and swaps it into the
// scheduler atomically. In-flight requests keep their previous world.
// Targets whose credential cannot be resolved yet (key not provisioned via
// env/file/store, or an OAuth account that has not completed its browser
// login) are compiled into the snapshot but get no egress caller: the
// scheduler excludes them with an actionable reason until their key is added
// or the login completes, either of which triggers a republish.
func publish(sch *sched.Scheduler, comp *policy.Compiler, f *config.File,
	resolver *config.Resolver, oauthMgr *oauth.Manager,
	inferenceClient func(connectionID string) *http.Client,
	apiKeys []domain.APIKeyAuthenticator, runtimeRevision int64) error {
	if inferenceClient == nil {
		return fmt.Errorf("inference HTTP client registry is required")
	}
	// Pin availability to the resolved account, rather than to a reusable name.
	// Refreshing an OAuth token on the same account keeps this identity stable.
	compiledConfig := *f
	compiledConfig.Connections = append([]domain.Connection(nil), f.Connections...)
	for i := range compiledConfig.Connections {
		c := &compiledConfig.Connections[i]
		binding := "unresolved"
		if account, ok := domain.OAuthAccountFromRef(c.Credential.SecretRef); ok {
			if oauthMgr != nil {
				if view, exists := oauthMgr.Account(account); exists {
					binding = "oauth:" + view.AccountID
				}
			}
		} else if _, _, native := nativeauth.ParseRef(c.Credential.SecretRef); native {
			if resolver != nil && resolver.NativeOAuth != nil {
				if identity, err := resolver.NativeOAuth.RuntimeIdentity(c.Credential.SecretRef); err == nil {
					binding = "native:" + identity
				}
			}
		} else if resolver != nil {
			if secret, err := resolver.Resolve(c.Credential.SecretRef); err == nil {
				binding = fmt.Sprintf("key:%x", sha256.Sum256([]byte(secret)))
			}
		}
		for _, provider := range f.Providers {
			if provider.ID == c.ProviderID {
				binding += fmt.Sprintf("|%s|%s", provider.Type, provider.BaseURL)
			}
		}
		c.RuntimeCredential = binding
	}
	snap, err := comp.CompileAtRevision(&compiledConfig, runtimeRevision)
	if err != nil {
		return err
	}
	callers := make(map[domain.ResourceTargetID]egress.Caller, len(snap.Targets))
	for _, id := range snap.TargetOrder {
		ct := snap.Targets[id]
		if ct == nil || ct.Disabled {
			continue
		}
		base := egress.Base{
			CodexClientVersion: f.EffectiveCodexClientVersion(),
			HTTP:               inferenceClient(ct.ConnectionID),
			Endpoint: &domain.Endpoint{
				ID:       ct.Endpoint.ID,
				BaseURL:  ct.Endpoint.BaseURL,
				Protocol: ct.Endpoint.Protocol,
			},
			CredRef:    ct.Credential.SecretRef,
			Credential: ct.Credential,
			Model:      ct.Model,
		}
		if account, isOAuth := domain.OAuthAccountFromRef(ct.Credential.SecretRef); isOAuth {
			// Built-in Codex account (one channel = one login): the credential
			// is its OAuth token, never a config secret. Until the browser
			// login completes the target stays excluded with that reason.
			_, _, err := oauthMgr.Identity(context.Background(), account)
			if err != nil {
				log.Printf("target %s: oauth account %q not ready, target disabled until login completes (%v)",
					id, account, err)
				continue
			}
			if status, ok := oauthMgr.Account(account); ok &&
				!domain.CodexModelSupportedForPlan(status.PlanType, ct.Model) {
				log.Printf("target %s: oauth account %q plan %q does not include model %q",
					id, account, status.PlanType, ct.Model)
				continue
			}
			// Resolve the access token and account identity at attempt time. The
			// compiled world pins only the oauth ref; lazy refresh can therefore
			// take effect without a config republish.
			oauthAccount := account
			targetProtocol := ct.Endpoint.Protocol
			callers[id] = egress.NewRefreshingDynamicCaller(base, func(ctx context.Context) (egress.CredentialValue, error) {
				accessToken, accountID, err := oauthMgr.Identity(ctx, oauthAccount)
				return egress.CredentialValue{Secret: accessToken, Identity: accountID}, err
			}, func(ctx context.Context, failed egress.CredentialValue) (egress.CredentialValue, error) {
				accessToken, accountID, err := oauthMgr.RefreshIdentity(ctx, oauthAccount, failed.Secret)
				return egress.CredentialValue{Secret: accessToken, Identity: accountID}, err
			}, func(current egress.Base, accountID string) egress.Caller {
				current.AfterResponse = func(headers http.Header) { oauthMgr.ObserveUsageHeaders(oauthAccount, accountID, headers) }
				current.BeforeSend = func(ctx context.Context) *domain.Failure { return oauthMgr.CheckUsage(ctx, oauthAccount, accountID) }
				if targetProtocol == domain.ProtocolOpenAIImages {
					return egress.NewCodexImages(current, accountID)
				}
				return egress.NewCodexCaller(current, accountID)
			})
			continue
		}

		if driver, account, native := nativeauth.ParseRef(ct.Credential.SecretRef); native {
			if resolver == nil || resolver.NativeOAuth == nil {
				continue
			}
			view, viewErr := resolver.NativeOAuth.Status(driver, account)
			if viewErr != nil || view.Status != "connected" {
				continue
			}
			if err := resolver.NativeOAuth.ValidateBinding(ct.Credential.SecretRef, ct.Endpoint.BaseURL, string(ct.Endpoint.Protocol)); err != nil {
				log.Printf("target %s: native account endpoint changed, target disabled until configuration is updated", id)
				continue
			}
			base.HTTP = resolver.NativeOAuth.ClientFor(ct.Credential.SecretRef, base.HTTP)
			base.Secret = "native-managed"
			callers[id] = egress.NewCaller(base, ct.Endpoint.Protocol)
			continue
		}
		secret, err := resolver.Resolve(ct.Credential.SecretRef)
		if err != nil {
			log.Printf("target %s: credential unavailable, target disabled until key is set (%v)",
				id, err)
			continue
		}
		base.Secret = secret
		if len(ct.NativeProtocols) > 1 {
			// Channel-expanded multi-protocol model: the caller switches wire
			// format per request (native passthrough, else least-lossy
			// native) instead of being pinned to one adapter.
			natives := domain.SortIDs(ct.NativeProtocols)
			callers[id] = egress.NewMultiCaller(base, natives, func(rc *domain.RequestContract) domain.Protocol {
				return ct.EgressProtocol(rc.Protocol)
			})
		} else {
			callers[id] = egress.NewCaller(base, ct.Endpoint.Protocol)
		}
	}
	cards := make([]*domain.RateCard, 0, len(f.RateCards))
	for _, rc := range f.RateCards {
		cards = append(cards, &domain.RateCard{
			Version: rc.Version, Provider: rc.Provider,
			Model: rc.Model, Plan: rc.Plan,
			InputPerM: rc.InputPerM, OutputPerM: rc.OutputPerM,
			CacheReadPerM: rc.CacheReadPerM, CacheWritePerM: rc.CacheWritePerM,
		})
	}
	sch.ReplaceWorldWithAPIKeys(snap, callers, cards, apiKeys)
	return nil
}

// channelModelCount counts models declared across all channels (used only for
// an accurate startup log line).
func channelModelCount(f *config.File) int {
	n := 0
	for i := range f.Channels {
		if f.Channels[i].EnabledOrDefault() {
			n += len(f.Channels[i].Models)
		}
	}
	return n
}
