package kiln

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"strconv"
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
	// Logger receives runtime diagnostics. Optional.
	Logger *slog.Logger
}

// Runtime starts, supervises and stops preview environments. It owns the port
// pool shared by all previews; the registry, concurrency cap, idle reaper and
// setup/teardown commands live one level up in the Kiln manager.
type Runtime struct {
	store       Store
	ports       *PortAllocator
	bindHost    string
	publicHost  string
	stopTimeout time.Duration
	lifetime    context.Context
	logger      *slog.Logger
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
		store:       cfg.Store,
		ports:       cfg.Ports,
		bindHost:    bindHost,
		publicHost:  publicHost,
		stopTimeout: stopTimeout,
		lifetime:    lifetime,
		logger:      logger,
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
	return p, nil
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

// EntryURL returns the URL of the entry service — the link an operator opens.
// It is empty when the manifest has no entry service (a manifest with more than
// one service and no `entry: true` never validates, so this only happens for an
// empty preview).
func (p *Preview) EntryURL() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, svc := range p.services {
		if svc.entry {
			return "http://" + net.JoinHostPort(p.runtime.publicHost, strconv.Itoa(svc.port)) + "/"
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
			Name:    svc.name,
			Port:    svc.port,
			Health:  svc.health,
			PID:     pid,
			LogPath: svc.logPath,
			Entry:   svc.entry,
			Error:   svc.failure,
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
func (p *Preview) deriveStatus() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	healthy, failed := 0, 0
	for _, svc := range p.services {
		switch svc.health {
		case state.PreviewServiceHealthy:
			healthy++
		case state.PreviewServiceFailed:
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
