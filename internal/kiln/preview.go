package kiln

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/state"
)

// Store is the persistence Kiln's runtime needs. *state.DB satisfies it.
type Store interface {
	UpsertPreview(p state.Preview) error
}

// The daemon passes its *state.DB here, so keep the two in step at compile time
// rather than at wiring time.
var _ Store = (*state.DB)(nil)

// RuntimeConfig configures a Runtime.
type RuntimeConfig struct {
	// Store persists preview records so they survive a daemon restart (and
	// feed the previews API). Optional: a nil store runs previews without
	// persistence, which is what the runtime tests use.
	Store Store
	// Ports allocates service ports from settings.preview_port_range.
	Ports *PortAllocator
	// BindHost is the address services are expected to bind (and are probed
	// on). Defaults to 127.0.0.1.
	BindHost string
	// PublicHost is the hostname used in URLs handed to services and shown to
	// operators. Defaults to BindHost.
	PublicHost string
	// StopTimeout is how long a service may take to exit on teardown before
	// its process group is killed. Zero uses DefaultStopTimeout.
	StopTimeout time.Duration
	// Lifetime ties every spawned process to a context — cancelling it kills
	// all previews. The daemon passes its run context here so shutdown cannot
	// leave preview processes behind. nil means context.Background().
	Lifetime context.Context
	// OnServiceExit is called when a service that had become healthy dies on its
	// own. It is how the death leaves this package: the runtime owns the state
	// transition and the persisted record, while announcing it — the feed event,
	// the operator-facing message — belongs to the daemon. Optional; a nil hook
	// still transitions and persists.
	//
	// It is never called for a service killed by teardown or shutdown, and never
	// for one that failed its readiness check (which is not a death after
	// readiness — Start already reported it). It runs on the supervisor's own
	// goroutine, so a slow implementation delays nothing but itself.
	OnServiceExit func(ServiceExit)
	// Logger receives runtime diagnostics. Optional.
	Logger *slog.Logger
}

// ServiceExit describes a preview service that became healthy and then died. It
// carries everything an operator-facing report needs without a second lookup:
// which preview, which service, why, how long it lived, and what the preview's
// status is now that a limb is gone.
type ServiceExit struct {
	BeadID  string
	Anvil   string
	Service string
	// Entry marks the service whose URL is *the* preview link — the case where
	// the death takes the whole preview offline as far as a browser is concerned.
	Entry bool
	// ExitCode is the process's exit status, or nil when it was killed by a
	// signal (Err then names the signal).
	ExitCode *int
	// Err is the wait error, nil for a service that exited cleanly.
	Err error
	// StartedAt and ExitedAt bound the service's life; Lifetime is their span.
	StartedAt time.Time
	ExitedAt  time.Time
	Lifetime  time.Duration
	// Status is the preview's overall status recomputed after the death, by the
	// same fold that decided it at startup.
	Status string
	// Detail is the rendered cause, e.g. `exited (exit 1, lived 7m31s)`.
	Detail string
}

// Runtime starts, supervises and stops preview environments. It owns the port
// pool shared by all previews; the registry, concurrency cap, idle reaper and
// setup/teardown commands live one level up in the Kiln manager.
type Runtime struct {
	store         Store
	ports         *PortAllocator
	bindHost      string
	publicHost    string
	stopTimeout   time.Duration
	lifetime      context.Context
	onServiceExit func(ServiceExit)
	logger        *slog.Logger
}

// NewRuntime returns a Runtime for the given configuration.
func NewRuntime(cfg RuntimeConfig) (*Runtime, error) {
	if cfg.Ports == nil {
		return nil, errors.New("kiln: runtime requires a port allocator")
	}
	bindHost := cfg.BindHost
	if bindHost == "" {
		bindHost = "127.0.0.1"
	}
	publicHost := cfg.PublicHost
	if publicHost == "" {
		publicHost = bindHost
	}
	lifetime := cfg.Lifetime
	if lifetime == nil {
		lifetime = context.Background()
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	stopTimeout := cfg.StopTimeout
	if stopTimeout <= 0 {
		stopTimeout = DefaultStopTimeout
	}
	return &Runtime{
		store:         cfg.Store,
		ports:         cfg.Ports,
		bindHost:      bindHost,
		publicHost:    publicHost,
		stopTimeout:   stopTimeout,
		lifetime:      lifetime,
		onServiceExit: cfg.OnServiceExit,
		logger:        logger,
	}, nil
}

// StartRequest describes the preview to start. The worktree must already exist
// (worktree.CreateDetached) and the manifest must come from the anvil's main
// checkout (kiln.Load).
type StartRequest struct {
	BeadID       string
	Anvil        string
	AnvilPath    string
	Branch       string
	WorktreePath string
	Manifest     *Manifest
	// Env is the environment services inherit; nil means the daemon's own.
	Env []string
	// Setup, when non-nil, runs after ports are allocated and the manifest has
	// been expanded but before any service is spawned. It exists as a callback
	// rather than as something the caller does first because the manifest's
	// setup command may reference `{{.ServicePort "name"}}` — it cannot be
	// expanded until the ports it names have been allocated, and allocation is
	// the runtime's job.
	//
	// A non-nil error aborts the start: ports are released, nothing is spawned,
	// and the error is returned. The persisted record is left behind on purpose
	// so whatever the setup command managed to create (a database, say) is
	// still visible to the caller's rollback and to startup reconciliation.
	Setup func(ctx context.Context, expanded *Manifest, env PreviewEnv) error
}

// Preview is a running preview environment: its allocated ports, supervised
// service processes and their health.
type Preview struct {
	BeadID       string
	Anvil        string
	AnvilPath    string
	Branch       string
	WorktreePath string
	// PreviewID is the sanitized bead id services see as FORGE_PREVIEW_ID.
	PreviewID string
	CreatedAt time.Time

	runtime *Runtime
	cancel  context.CancelFunc
	// procCtx is the context every service process runs on. Its cancellation is
	// how the exit watchers tell a teardown apart from a service dying on its
	// own: the same process death means opposite things either side of it.
	procCtx context.Context

	mu       sync.Mutex
	status   string
	services []*previewService
	stopped  bool
}

// previewService is the runtime state of one service in a preview.
type previewService struct {
	name    string
	port    int
	entry   bool
	health  string
	failure string
	logPath string
	proc    *ServiceProcess
	// startedAt and exitedAt bound the process's life. exitedAt is zero while it
	// runs, which is what freezes a dead service's uptime — see
	// state.PreviewService.Lifetime, the one place the interval is turned into a
	// duration.
	startedAt time.Time
	exitedAt  time.Time
	// exitCode is the process's exit status, nil while it runs and for a
	// signalled process (which has none).
	exitCode *int
}

// Start allocates ports, runs the request's setup callback, spawns every
// service and waits for their health checks, then returns the resulting
// preview.
//
// It blocks until every service is healthy or has failed, so the caller decides
// how to report progress (the web layer runs it behind the 202 + request_id
// pattern). A service that fails never stops its siblings: the preview comes
// back as degraded with per-service detail, because a broken client is no
// reason to throw away a working api.
//
// A non-nil error means nothing is running — ports are released and any process
// already spawned has been killed. ctx bounds the start; the processes
// themselves live on the runtime's lifetime context.
func (r *Runtime) Start(ctx context.Context, req StartRequest) (*Preview, error) {
	if req.Manifest == nil {
		return nil, errors.New("kiln: start requires a manifest")
	}
	if len(req.Manifest.Services) == 0 {
		return nil, errors.New("kiln: manifest declares no services")
	}
	if req.BeadID == "" {
		return nil, errors.New("kiln: start requires a bead id")
	}
	if req.WorktreePath == "" {
		return nil, errors.New("kiln: start requires a preview worktree path")
	}
	// Fail once, clearly, rather than once per service: a missing checkout is a
	// caller bug (worktree.CreateDetached was skipped or failed), not a broken
	// app.
	info, err := os.Stat(req.WorktreePath)
	if err != nil {
		return nil, fmt.Errorf("kiln: preview worktree %s: %w", req.WorktreePath, err)
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("kiln: preview worktree %s is not a directory", req.WorktreePath)
	}

	allocated, err := r.ports.AllocateN(len(req.Manifest.Services))
	if err != nil {
		return nil, fmt.Errorf("kiln: allocating ports for preview of %s: %w", req.BeadID, err)
	}
	ports := make(map[string]int, len(allocated))
	for i, svc := range req.Manifest.Services {
		ports[svc.Name] = allocated[i]
	}

	previewID := SanitizePreviewID(req.BeadID)
	expanded, err := req.Manifest.Expand(Context{
		PreviewID: previewID,
		Host:      r.publicHost,
		BindHost:  r.bindHost,
		Ports:     ports,
	})
	if err != nil {
		r.ports.Release(allocated...)
		return nil, fmt.Errorf("kiln: expanding manifest for preview of %s: %w", req.BeadID, err)
	}

	p := &Preview{
		BeadID:       req.BeadID,
		Anvil:        req.Anvil,
		AnvilPath:    req.AnvilPath,
		Branch:       req.Branch,
		WorktreePath: req.WorktreePath,
		PreviewID:    previewID,
		CreatedAt:    time.Now(),
		runtime:      r,
		status:       state.PreviewStarting,
	}
	for _, svc := range expanded.Services {
		logPath, err := ServiceLogPath(req.BeadID, svc.Name)
		if err != nil {
			r.ports.Release(allocated...)
			return nil, err
		}
		p.services = append(p.services, &previewService{
			name:    svc.Name,
			port:    ports[svc.Name],
			entry:   svc.Entry,
			health:  state.PreviewServiceStarting,
			logPath: logPath,
		})
	}

	// Persist before spawning: a daemon that dies mid-start must leave a record
	// the reconciler can find, not an invisible set of orphan processes.
	if err := r.persist(p); err != nil {
		r.ports.Release(allocated...)
		return nil, err
	}

	env := PreviewEnv{
		PreviewID:    previewID,
		BeadID:       req.BeadID,
		Branch:       req.Branch,
		WorktreePath: req.WorktreePath,
		AnvilName:    req.Anvil,
		AnvilPath:    req.AnvilPath,
		Ports:        ports,
	}

	// Setup runs before the first service so a project can create the resources
	// its services expect to find (a per-preview database being the motivating
	// case). A failure here means the preview cannot work, so nothing is
	// spawned at all.
	if req.Setup != nil {
		if err := req.Setup(ctx, expanded, env); err != nil {
			r.ports.Release(allocated...)
			p.setStatus(state.PreviewFailed)
			if perr := r.persist(p); perr != nil {
				r.logger.Warn("kiln: persisting failed preview record failed", "bead", req.BeadID, "error", perr)
			}
			return nil, err
		}
	}

	procCtx, cancel := context.WithCancel(r.lifetime)
	p.cancel = cancel
	p.procCtx = procCtx

	// Spawn in manifest order — it is the order the author wrote and costs
	// nothing, since starting a process does not wait for it to be usable.
	for i, svc := range expanded.Services {
		proc, err := StartService(procCtx, ServiceSpec{
			Service:      svc,
			BeadID:       req.BeadID,
			WorktreePath: req.WorktreePath,
			Env:          BuildEnv(req.Env, env, svc.Env),
			Logger:       r.logger,
		})
		if err != nil {
			// A service that cannot even be spawned is just a failed service;
			// the rest of the preview is still worth having.
			r.logger.Warn("kiln: preview service failed to start",
				"bead", req.BeadID, "service", svc.Name, "error", err)
			p.markFailed(i, err)
			continue
		}
		p.setProcess(i, proc)
	}

	p.waitHealthy(ctx, expanded)

	if ctx.Err() != nil {
		// The caller gave up (or the daemon is shutting down) — do not leave
		// half-started processes behind.
		_ = p.Stop()
		return nil, fmt.Errorf("kiln: starting preview of %s: %w", req.BeadID, ctx.Err())
	}

	p.setStatus(p.deriveStatus())
	if err := r.persist(p); err != nil {
		// The preview is up; losing the record only costs the reconciler, so
		// this must not fail the start.
		r.logger.Warn("kiln: persisting preview record failed", "bead", req.BeadID, "error", err)
	}
	// Only now, with every readiness check settled: a watcher started earlier
	// would race the health check for the same service and could demote one that
	// is about to be marked healthy, or mark exited something the check is about
	// to call failed. Starting them here means each service's health is already
	// final, so the watcher only ever sees the one transition it owns.
	p.watchServiceExits()
	return p, nil
}

// watchServiceExits starts one goroutine per running service, each waiting for
// its process to be reaped and then handing the death to handleServiceExit.
//
// A service whose process is already gone is not a special case: its Done
// channel is closed, so its watcher runs immediately.
func (p *Preview) watchServiceExits() {
	p.mu.Lock()
	defer p.mu.Unlock()
	for i, svc := range p.services {
		if svc.proc == nil {
			continue
		}
		go func(i int, proc *ServiceProcess) {
			<-proc.Done()
			p.handleServiceExit(i)
		}(i, svc.proc)
	}
}

// handleServiceExit consumes one service's death.
//
// The supervisor has always known when a service exits — it writes the fact
// into the service log — but nothing read it back, so a dev server that died
// seven minutes after a clean start left every surface reporting `healthy` over
// a dead process. This is the read-back: the service goes healthy → exited with
// its code, its uptime freezes at the exit, the preview's status is recomputed
// by the same fold that decided it at startup, the record is persisted, and the
// runtime's OnServiceExit hook lets the daemon announce it.
//
// A death during teardown or shutdown is recorded but never demoted: the
// process exiting *is* the teardown working, and reporting a preview as
// degraded on its way out would be noise on every stop. A service that never
// became healthy is left alone too — its readiness check already had the final
// word, and re-reporting the same failure under a second name helps nobody.
func (p *Preview) handleServiceExit(i int) {
	svc := p.serviceAt(i)
	if svc == nil || svc.proc == nil {
		return
	}
	exitedAt := time.Now()
	exitErr := svc.proc.ExitErr()
	var code *int
	if c, ok := svc.proc.ExitCode(); ok {
		code = &c
	}

	p.mu.Lock()
	// The exit is a fact either way, so record it even during teardown: it is
	// what stops a stopped preview's last snapshot claiming a still-growing
	// uptime.
	if svc.exitedAt.IsZero() {
		svc.exitedAt = exitedAt
		svc.exitCode = code
	}
	intentional := p.stopped || (p.procCtx != nil && p.procCtx.Err() != nil)
	demote := !intentional && svc.health == state.PreviewServiceHealthy
	lifetime := time.Duration(0)
	if !svc.startedAt.IsZero() {
		lifetime = svc.exitedAt.Sub(svc.startedAt)
	}
	detail := FormatServiceExit(svc.exitCode, exitErr, lifetime)
	if demote {
		svc.health = state.PreviewServiceExited
		svc.failure = detail
	}
	name, entry, startedAt := svc.name, svc.entry, svc.startedAt
	p.mu.Unlock()

	if !demote {
		return
	}

	status := p.deriveStatus()
	p.setStatus(status)
	if err := p.runtime.persist(p); err != nil {
		p.runtime.logger.Warn("kiln: persisting a preview service exit failed",
			"bead", p.BeadID, "service", name, "error", err)
	}
	p.runtime.logger.Warn("kiln: preview service exited after becoming healthy",
		"bead", p.BeadID, "service", name, "entry", entry,
		"detail", detail, "preview_status", status)

	if hook := p.runtime.onServiceExit; hook != nil {
		hook(ServiceExit{
			BeadID:    p.BeadID,
			Anvil:     p.Anvil,
			Service:   name,
			Entry:     entry,
			ExitCode:  code,
			Err:       exitErr,
			StartedAt: startedAt,
			ExitedAt:  exitedAt,
			Lifetime:  lifetime,
			Status:    status,
			Detail:    detail,
		})
	}
}

// waitHealthy runs every service's readiness check concurrently and records the
// outcome per service.
func (p *Preview) waitHealthy(ctx context.Context, expanded *Manifest) {
	var wg sync.WaitGroup
	for i, svc := range expanded.Services {
		ps := p.serviceAt(i)
		if ps == nil || ps.proc == nil {
			continue // never started; already marked failed
		}
		wg.Add(1)
		go func(i int, svc Service, proc *ServiceProcess) {
			defer wg.Done()
			check := HealthCheck{
				Host:    p.runtime.bindHost,
				Port:    p.portAt(i),
				Path:    svc.Health,
				Timeout: svc.ReadyTimeout,
				Exited:  proc.Done(),
			}
			if err := check.Wait(ctx); err != nil {
				p.runtime.logger.Warn("kiln: preview service failed its health check",
					"bead", p.BeadID, "service", svc.Name, "port", p.portAt(i), "error", err)
				p.markFailed(i, err)
				return
			}
			p.markHealthy(i)
		}(i, svc, ps.proc)
	}
	wg.Wait()
}

// Stop terminates every service in the preview and releases its ports. It does
// not remove the worktree or run the manifest's teardown command — those are
// the manager's, and they must happen after the processes are gone.
//
// Stop is idempotent and safe to call on a preview that never fully started.
// It is bounded by the runtime's stop timeout rather than a context: teardown
// must finish even when the context that triggered it is already cancelled.
func (p *Preview) Stop() error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	services := append([]*previewService(nil), p.services...)
	p.mu.Unlock()

	timeout := p.runtime.stopTimeout
	errsCh := make(chan error, len(services))
	var wg sync.WaitGroup
	for _, svc := range services {
		if svc.proc == nil {
			continue
		}
		wg.Add(1)
		go func(svc *previewService) {
			defer wg.Done()
			if err := svc.proc.Stop(timeout); err != nil {
				errsCh <- fmt.Errorf("stopping %s: %w", svc.name, err)
			}
		}(svc)
	}
	wg.Wait()
	close(errsCh)

	if p.cancel != nil {
		p.cancel()
	}
	p.releasePorts()

	var errs []error
	for err := range errsCh {
		errs = append(errs, err)
	}

	p.setStatus(state.PreviewStopped)
	if err := p.runtime.persist(p); err != nil {
		p.runtime.logger.Warn("kiln: persisting stopped preview failed", "bead", p.BeadID, "error", err)
	}
	if len(errs) > 0 {
		return fmt.Errorf("kiln: stopping preview of %s: %w", p.BeadID, errors.Join(errs...))
	}
	return nil
}

// Status returns the preview's overall status (see the state.Preview* values).
func (p *Preview) Status() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.status
}

// EntryURL returns the address the entry service actually answers on:
// `http://<public host>:<port>/`. It is empty when the manifest has no entry
// service (a manifest with more than one service and no `entry: true` never
// validates, so this only happens for an empty preview), and when the entry
// service is not serving — a link to a process that has exited is worse than no
// link, since the browser error it produces looks like a network problem.
//
// This is the *direct* address, which is what something running on this host
// needs — the preview quest runner drives a headless browser at it, and the
// daemon logs it. The link an *operator* opens may be a preview hostname
// instead (settings.preview_proxy_base); that one depends on daemon settings
// this runtime does not read, so it is built one level up by the daemon's
// previewEntryURL. Both go through kiln.EntryURL.
func (p *Preview) EntryURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, svc := range p.services {
		if svc.entry {
			if !state.PreviewServiceServing(svc.health) {
				return ""
			}
			return EntryURL(EntryURLOptions{Host: p.runtime.publicHost, Port: svc.port})
		}
	}
	return ""
}

// Record returns the persistable snapshot of this preview.
func (p *Preview) Record() state.Preview {
	p.mu.Lock()
	defer p.mu.Unlock()
	rec := state.Preview{
		BeadID:       p.BeadID,
		Anvil:        p.Anvil,
		Branch:       p.Branch,
		Status:       p.status,
		WorktreePath: p.WorktreePath,
		CreatedAt:    p.CreatedAt,
		LastActiveAt: time.Now(),
		Services:     make([]state.PreviewService, 0, len(p.services)),
	}
	for _, svc := range p.services {
		// Only report a PID while the process is actually alive: the record is
		// what a restarted daemon reconciles against, and killing a PID the OS
		// has since handed to something else is worse than missing a stray.
		pid := 0
		if svc.proc != nil && !svc.proc.Exited() {
			pid = svc.proc.PID()
		}
		rec.Services = append(rec.Services, state.PreviewService{
			Name:      svc.name,
			Port:      svc.port,
			Health:    svc.health,
			PID:       pid,
			LogPath:   svc.logPath,
			Entry:     svc.entry,
			Error:     svc.failure,
			StartedAt: svc.startedAt,
			ExitedAt:  svc.exitedAt,
			ExitCode:  svc.exitCode,
		})
	}
	return rec
}

// Ports returns the ports allocated to this preview, in manifest order.
func (p *Preview) Ports() []int {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]int, 0, len(p.services))
	for _, svc := range p.services {
		out = append(out, svc.port)
	}
	return out
}

// persist writes the preview's current snapshot. A nil store is a no-op.
func (r *Runtime) persist(p *Preview) error {
	if r.store == nil {
		return nil
	}
	if err := r.store.UpsertPreview(p.Record()); err != nil {
		return fmt.Errorf("kiln: persisting preview %s: %w", p.BeadID, err)
	}
	return nil
}

// deriveStatus folds the per-service health into the preview's overall status.
//
// It is the single rule set, applied both when a start finishes and whenever a
// service dies afterwards, which is what keeps "one service down" meaning the
// same thing at minute zero and at minute seven. An exited service counts as
// failed here: for the fold, all that matters is that it is not serving.
func (p *Preview) deriveStatus() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	healthy, failed := 0, 0
	for _, svc := range p.services {
		switch svc.health {
		case state.PreviewServiceHealthy:
			healthy++
		case state.PreviewServiceFailed, state.PreviewServiceExited:
			failed++
		}
	}
	switch {
	case healthy == 0:
		return state.PreviewFailed
	case failed > 0:
		return state.PreviewDegraded
	case healthy == len(p.services):
		return state.PreviewRunning
	default:
		// Some service is still 'starting' — only reachable if a health check
		// was skipped, so report the honest in-between state.
		return state.PreviewStarting
	}
}

func (p *Preview) setStatus(status string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.status = status
}

func (p *Preview) setProcess(i int, proc *ServiceProcess) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.services[i].proc = proc
	p.services[i].logPath = proc.LogPath
	// Per-process rather than per-preview: services are spawned in manifest
	// order and a slow one starts measurably later, so uptime measured from the
	// preview's creation is already an approximation — and once a service is
	// restarted or dies, an approximation is the wrong thing to report.
	p.services[i].startedAt = proc.StartedAt
}

func (p *Preview) markHealthy(i int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.services[i].health = state.PreviewServiceHealthy
	p.services[i].failure = ""
}

func (p *Preview) markFailed(i int, err error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.services[i].health = state.PreviewServiceFailed
	if err != nil {
		p.services[i].failure = err.Error()
	}
}

func (p *Preview) serviceAt(i int) *previewService {
	p.mu.Lock()
	defer p.mu.Unlock()
	if i < 0 || i >= len(p.services) {
		return nil
	}
	return p.services[i]
}

func (p *Preview) portAt(i int) int {
	svc := p.serviceAt(i)
	if svc == nil {
		return 0
	}
	return svc.port
}

// releasePorts hands this preview's ports back to the shared pool.
func (p *Preview) releasePorts() {
	p.runtime.ports.Release(p.Ports()...)
}
