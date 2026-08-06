package kiln

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/Robin831/Forge/internal/config"
	"github.com/Robin831/Forge/internal/state"
	"github.com/Robin831/Forge/internal/worktree"
)

// ErrTooManyPreviews is what a start rejected by the concurrency cap wraps.
// Callers use errors.Is to tell "the box is full" (retry after stopping one)
// apart from "this preview is broken".
var ErrTooManyPreviews = errors.New("kiln: preview limit reached")

// TooManyPreviewsError reports a start refused because preview_max_concurrent
// is already reached. v1 rejects rather than queues: a preview is an
// interactive thing an operator asked for, and silently starting it ten minutes
// later — after they gave up and stopped watching — is worse than saying no now.
type TooManyPreviewsError struct {
	// Running is how many previews were up or starting when the request came in.
	Running int
	// Limit is the configured preview_max_concurrent.
	Limit int
}

func (e *TooManyPreviewsError) Error() string {
	return fmt.Sprintf("kiln: preview limit reached (%d of %d in use, preview_max_concurrent=%d) — stop a running preview first",
		e.Running, e.Limit, e.Limit)
}

// Unwrap makes errors.Is(err, ErrTooManyPreviews) work.
func (e *TooManyPreviewsError) Unwrap() error { return ErrTooManyPreviews }

// ManagerStore is the persistence the Manager needs: the runtime's Store (which
// writes the row while a preview starts and stops) plus the row-level
// operations teardown and the idle reaper depend on.
type ManagerStore interface {
	Store
	TouchPreview(beadID string) (bool, error)
	DeletePreview(beadID string) error
}

// The daemon passes its *state.DB here, so keep the two in step at compile time.
var _ ManagerStore = (*state.DB)(nil)

// Instance is a started preview's process side — what Runner.Start hands back.
// *Preview satisfies it; tests substitute a fake.
type Instance interface {
	// Stop terminates every service and releases its ports. Idempotent.
	Stop() error
	// Status is the preview's overall status (see the state.Preview* values).
	Status() string
	// EntryURL is the link an operator opens, or "" when there is none.
	EntryURL() string
	// Ports are the ports allocated to this preview, in manifest order.
	Ports() []int
	// Record is the persistable snapshot of the preview.
	Record() state.Preview
}

// Runner starts preview service processes. *Runtime satisfies it via
// RuntimeRunner.
type Runner interface {
	Start(ctx context.Context, req StartRequest) (Instance, error)
}

// Worktrees materializes and removes the detached preview checkouts under
// <anvil>/.previews/. GitWorktrees is the real implementation; tests use a fake
// so the manager's flows can be exercised without a git repository.
type Worktrees interface {
	// CreateDetached checks branch out at <anvilPath>/.previews/<beadID> with a
	// detached HEAD and returns the resulting path.
	CreateDetached(ctx context.Context, anvilPath, beadID, branch string) (string, error)
	// RemoveDetached tears that checkout down. Removing one that is not there
	// is not an error.
	RemoveDetached(ctx context.Context, anvilPath, beadID string) error
}

// GitWorktrees is the Worktrees implementation backed by the real git helpers
// in internal/worktree.
type GitWorktrees struct{}

// CreateDetached implements Worktrees.
func (GitWorktrees) CreateDetached(ctx context.Context, anvilPath, beadID, branch string) (string, error) {
	wt, err := worktree.CreateDetached(ctx, anvilPath, beadID, branch)
	if err != nil {
		return "", err
	}
	return wt.Path, nil
}

// RemoveDetached implements Worktrees.
func (GitWorktrees) RemoveDetached(ctx context.Context, anvilPath, beadID string) error {
	return worktree.RemoveDetached(ctx, anvilPath, beadID)
}

// ManagerConfig is the Kiln manager's slice of settings.
type ManagerConfig struct {
	// MaxConcurrent caps how many previews may run at once
	// (settings.preview_max_concurrent). Zero uses the configured default.
	MaxConcurrent int
	// IdleTimeout is how long a preview may go untouched before the reaper
	// tears it down (settings.preview_idle_timeout). Zero disables reaping.
	// The manager records the deadline; the reaper acts on it.
	IdleTimeout time.Duration
	// CommandTimeout bounds the manifest's setup and teardown commands. Zero
	// uses DefaultCommandTimeout.
	CommandTimeout time.Duration
	// PublicHost is the hostname manifest templates expand {{.Host}} against.
	// It must match the runtime's, so a teardown command builds the same URL
	// its services were handed. Zero value uses the loopback default.
	PublicHost string
	// Env is the environment preview commands inherit; nil means the daemon's
	// own (os.Environ).
	Env []string
}

// ManagerDeps is everything NewManager needs. Runtime and Worktrees are
// interfaces so the manager's flows are testable without spawning processes or
// touching git; Store may be nil, which runs previews without persistence.
type ManagerDeps struct {
	Runtime   Runner
	Worktrees Worktrees
	Store     ManagerStore
	Config    ManagerConfig
	Logger    *slog.Logger
	// LoadManifest reads an anvil's preview manifest. Defaults to Load, which
	// reads it from the anvil's MAIN checkout — never the PR branch, because
	// the manifest decides which commands run on the host.
	LoadManifest func(anvilPath string) (*Manifest, error)
	// Now is the clock, injectable so idle behaviour is testable. Defaults to
	// time.Now.
	Now func() time.Time
}

// Manager owns the set of running preview environments: the registry, the
// concurrency cap, the worktree and setup/teardown lifecycle around what the
// Runtime supervises, and the state.db rows the reaper and startup
// reconciliation read.
//
// Concurrency model, since two locks look like one too many:
//   - mu guards the registry and the in-flight set. It is never held across
//     slow work (git, setup scripts, process spawning, health checks).
//   - a per-bead lock serializes Start and Stop for the same bead, so a second
//     Start blocks and then returns the first one's preview instead of racing
//     it into a duplicate worktree, and a Stop cannot delete the worktree a
//     Start is still filling.
type Manager struct {
	runtime      Runner
	worktrees    Worktrees
	store        ManagerStore
	cfg          ManagerConfig
	logger       *slog.Logger
	loadManifest func(anvilPath string) (*Manifest, error)
	now          func() time.Time

	mu sync.Mutex
	// envs holds the previews that finished starting, by bead id.
	envs map[string]*Environment
	// starting holds the beads whose Start is in flight. It reserves a slot
	// against MaxConcurrent so two concurrent starts cannot both pass a cap
	// check that only counts finished previews.
	starting map[string]bool
	// locks are the per-bead serializers, reference counted so the map does not
	// grow with every bead ever previewed.
	locks map[string]*beadLock
}

// beadLock serializes Start/Stop for one bead.
type beadLock struct {
	mu   sync.Mutex
	refs int
}

// StartOptions identifies the preview to start. The manager takes the anvil and
// branch explicitly rather than resolving them from the bead id: resolution
// belongs to the daemon, which already holds the config and the PR state.
type StartOptions struct {
	// BeadID is the bead the preview belongs to; it keys the registry, the
	// state row, the worktree directory and the log directory.
	BeadID string
	// Anvil is the anvil's configured name.
	Anvil string
	// AnvilPath is the anvil's MAIN checkout — the manifest is read from there.
	AnvilPath string
	// Branch is the branch to preview.
	Branch string
}

func (o StartOptions) validate() error {
	switch {
	case strings.TrimSpace(o.BeadID) == "":
		return errors.New("kiln: starting a preview requires a bead id")
	case strings.TrimSpace(o.AnvilPath) == "":
		return errors.New("kiln: starting a preview requires an anvil path")
	case strings.TrimSpace(o.Branch) == "":
		return errors.New("kiln: starting a preview requires a branch")
	}
	return nil
}

// Environment is one preview the manager owns: the supervised services plus the
// worktree, manifest and timestamps around them.
type Environment struct {
	// BeadID, Anvil, AnvilPath, Branch and WorktreePath describe what is being
	// previewed and where it lives. They are set once and never change.
	BeadID       string
	Anvil        string
	AnvilPath    string
	Branch       string
	WorktreePath string
	// CreatedAt is when the preview started.
	CreatedAt time.Time

	manifest *Manifest
	instance Instance

	mu         sync.Mutex
	lastActive time.Time
}

// Status returns the preview's overall status.
func (e *Environment) Status() string {
	if e.instance == nil {
		return state.PreviewStarting
	}
	return e.instance.Status()
}

// EntryURL returns the link to the preview's entry service.
func (e *Environment) EntryURL() string {
	if e.instance == nil {
		return ""
	}
	return e.instance.EntryURL()
}

// Ports returns the ports allocated to this preview, in manifest order.
func (e *Environment) Ports() []int {
	if e.instance == nil {
		return nil
	}
	return e.instance.Ports()
}

// LastActive returns when this preview was last touched — what the idle reaper
// measures against.
func (e *Environment) LastActive() time.Time {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.lastActive
}

// Record returns the persistable snapshot of this preview.
func (e *Environment) Record() state.Preview {
	rec := state.Preview{
		BeadID:       e.BeadID,
		Anvil:        e.Anvil,
		Branch:       e.Branch,
		Status:       state.PreviewStarting,
		WorktreePath: e.WorktreePath,
		CreatedAt:    e.CreatedAt,
	}
	if e.instance != nil {
		rec = e.instance.Record()
	}
	rec.LastActiveAt = e.LastActive()
	return rec
}

func (e *Environment) touch(now time.Time) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.lastActive = now
}

// NewManager returns a Manager for the given dependencies.
func NewManager(deps ManagerDeps) (*Manager, error) {
	if deps.Runtime == nil {
		return nil, errors.New("kiln: manager requires a runtime")
	}
	if deps.Worktrees == nil {
		return nil, errors.New("kiln: manager requires a worktree provider")
	}
	logger := deps.Logger
	if logger == nil {
		logger = slog.Default()
	}
	load := deps.LoadManifest
	if load == nil {
		load = Load
	}
	now := deps.Now
	if now == nil {
		now = time.Now
	}
	cfg := deps.Config
	if cfg.MaxConcurrent <= 0 {
		cfg.MaxConcurrent = config.DefaultPreviewMaxConcurrent
	}
	if cfg.CommandTimeout <= 0 {
		cfg.CommandTimeout = DefaultCommandTimeout
	}
	if strings.TrimSpace(cfg.PublicHost) == "" {
		cfg.PublicHost = config.DefaultPreviewBindHost
	}
	return &Manager{
		runtime:      deps.Runtime,
		worktrees:    deps.Worktrees,
		store:        deps.Store,
		cfg:          cfg,
		logger:       logger,
		loadManifest: load,
		now:          now,
		envs:         make(map[string]*Environment),
		starting:     make(map[string]bool),
		locks:        make(map[string]*beadLock),
	}, nil
}

// MaxConcurrent returns the effective concurrency cap.
func (m *Manager) MaxConcurrent() int { return m.cfg.MaxConcurrent }

// IdleTimeout returns the configured idle timeout (0 = reaping disabled).
func (m *Manager) IdleTimeout() time.Duration { return m.cfg.IdleTimeout }

// Start brings up the preview for a bead and returns it.
//
// It is idempotent: a bead that already has a preview gets that preview back
// (touched, so an operator re-opening the link keeps it alive) without a second
// worktree or a second set of processes.
//
// The order is worktree → setup → services → health, and everything after the
// worktree is created unwinds on failure: services killed, teardown run,
// checkout removed, row deleted. A half-started preview is worse than none —
// it holds a port range and a worktree while serving nothing.
func (m *Manager) Start(ctx context.Context, opts StartOptions) (*Environment, error) {
	if err := opts.validate(); err != nil {
		return nil, err
	}

	unlock := m.lockBead(opts.BeadID)
	defer unlock()

	// Reserve under the registry lock: the existence check and the cap check
	// have to be one atomic decision, or two starts racing for the last slot
	// both win it.
	existing, err := m.reserve(opts.BeadID)
	if err != nil {
		return nil, err
	}
	if existing != nil {
		m.Touch(opts.BeadID)
		return existing, nil
	}
	defer m.release(opts.BeadID)

	manifest, err := m.loadManifest(opts.AnvilPath)
	if err != nil {
		return nil, err
	}

	worktreePath, err := m.worktrees.CreateDetached(ctx, opts.AnvilPath, opts.BeadID, opts.Branch)
	if err != nil {
		return nil, fmt.Errorf("kiln: creating preview worktree for %s: %w", opts.BeadID, err)
	}

	env := &Environment{
		BeadID:       opts.BeadID,
		Anvil:        opts.Anvil,
		AnvilPath:    opts.AnvilPath,
		Branch:       opts.Branch,
		WorktreePath: worktreePath,
		CreatedAt:    m.now(),
		manifest:     manifest,
		lastActive:   m.now(),
	}

	inst, err := m.runtime.Start(ctx, StartRequest{
		BeadID:       opts.BeadID,
		Anvil:        opts.Anvil,
		AnvilPath:    opts.AnvilPath,
		Branch:       opts.Branch,
		WorktreePath: worktreePath,
		Manifest:     manifest,
		Env:          m.cfg.Env,
		Setup: func(ctx context.Context, expanded *Manifest, penv PreviewEnv) error {
			return m.runLifecycle(ctx, "setup", expanded.Setup, env, penv)
		},
	})
	if err != nil {
		m.rollback(ctx, env)
		return nil, err
	}
	env.instance = inst

	// A preview where nothing came up is a failed start, not a running
	// environment: unwind it so the ports, the worktree and the row are not
	// held by something that serves no page. Per-service logs survive under
	// ~/.forge/logs/<beadID>/ for the post-mortem.
	if status := inst.Status(); status == state.PreviewFailed {
		detail := failureDetail(inst.Record())
		m.rollback(ctx, env)
		return nil, fmt.Errorf("kiln: preview of %s failed to start: %s", opts.BeadID, detail)
	}

	env.touch(m.now())
	m.commit(env)
	m.touchStore(opts.BeadID)
	return env, nil
}

// Stop tears the preview for a bead down: services first, then the manifest's
// teardown command, then the checkout, then the row.
//
// Stopping a bead with no preview is a no-op, and so is a second concurrent
// Stop — the registry entry is removed before any of the slow work begins, so
// exactly one caller does the teardown. Every step runs even when an earlier
// one failed; the accumulated errors are joined and returned, because leaving a
// worktree behind because a teardown script exited 1 helps nobody.
func (m *Manager) Stop(ctx context.Context, beadID string) error {
	unlock := m.lockBead(beadID)
	defer unlock()

	m.mu.Lock()
	env, ok := m.envs[beadID]
	delete(m.envs, beadID)
	m.mu.Unlock()
	if !ok {
		return nil
	}

	return m.teardown(ctx, env)
}

// Touch bumps a preview's last-active time, in memory and in the row the idle
// reaper reads. It is called when a preview starts and whenever the preview API
// is used, and is a no-op for a bead with no preview.
func (m *Manager) Touch(beadID string) {
	m.mu.Lock()
	env, ok := m.envs[beadID]
	m.mu.Unlock()
	if !ok {
		return
	}
	env.touch(m.now())
	m.touchStore(beadID)
}

// Get returns the preview for a bead.
func (m *Manager) Get(beadID string) (*Environment, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	env, ok := m.envs[beadID]
	return env, ok
}

// List returns every running preview, ordered by bead id so callers (and their
// tests) get a stable list.
func (m *Manager) List() []*Environment {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]*Environment, 0, len(m.envs))
	for _, env := range m.envs {
		out = append(out, env)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].BeadID < out[j].BeadID })
	return out
}

// reserve takes a slot for beadID under the registry lock. It returns the
// existing preview when the bead already has one (the caller returns it
// unchanged), or a *TooManyPreviewsError when the cap is reached.
func (m *Manager) reserve(beadID string) (*Environment, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if env, ok := m.envs[beadID]; ok {
		return env, nil
	}
	// In-flight starts count: they already hold a worktree and (soon) ports.
	inUse := len(m.envs) + len(m.starting)
	if inUse >= m.cfg.MaxConcurrent {
		return nil, &TooManyPreviewsError{Running: inUse, Limit: m.cfg.MaxConcurrent}
	}
	m.starting[beadID] = true
	return nil, nil
}

// release drops the in-flight reservation for beadID.
func (m *Manager) release(beadID string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.starting, beadID)
}

// commit publishes a fully started preview into the registry.
func (m *Manager) commit(env *Environment) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.envs[env.BeadID] = env
}

// rollback unwinds a start that failed after the worktree was created. It is
// the same teardown Stop performs, with the errors logged rather than returned:
// the caller already has the failure that matters, and burying it under
// "and also the worktree removal complained" helps nobody.
func (m *Manager) rollback(ctx context.Context, env *Environment) {
	if err := m.teardown(ctx, env); err != nil {
		m.logger.Warn("kiln: unwinding a failed preview start hit errors",
			"bead", env.BeadID, "error", err)
	}
}

// teardown performs the full stop sequence for a preview that is already out of
// the registry. Every step runs regardless of what the previous one returned.
func (m *Manager) teardown(ctx context.Context, env *Environment) error {
	// Teardown must complete even when the context that triggered it is already
	// cancelled — daemon shutdown and an abandoned HTTP request both look like
	// that, and both still have to release the database, the ports and the
	// checkout. Every step is bounded by its own timeout instead.
	ctx = context.WithoutCancel(ctx)

	var errs []error

	// Processes first: teardown scripts expect the services to be gone (they
	// drop the database those services hold connections to), and on Windows a
	// running service holds file locks that make the worktree unremovable.
	if env.instance != nil {
		if err := env.instance.Stop(); err != nil {
			errs = append(errs, err)
		}
	}

	// Teardown is best-effort by design: a project's cleanup script failing
	// must not strand a worktree and a registry slot. The error is reported,
	// not obeyed.
	if env.manifest != nil && strings.TrimSpace(env.manifest.Teardown) != "" {
		if err := m.runTeardown(ctx, env); err != nil {
			m.logger.Warn("kiln: preview teardown command failed",
				"bead", env.BeadID, "error", err)
			errs = append(errs, err)
		}
	}

	if err := m.worktrees.RemoveDetached(ctx, env.AnvilPath, env.BeadID); err != nil {
		errs = append(errs, fmt.Errorf("kiln: removing preview worktree for %s: %w", env.BeadID, err))
	}

	if m.store != nil {
		if err := m.store.DeletePreview(env.BeadID); err != nil {
			errs = append(errs, err)
		}
	}

	return errors.Join(errs...)
}

// runTeardown expands the manifest against the preview's actual ports and runs
// its teardown command. Expansion is repeated here rather than cached from the
// start because a rollback can happen before the runtime ever expanded anything.
func (m *Manager) runTeardown(ctx context.Context, env *Environment) error {
	ports := m.portMap(env)
	expanded, err := env.manifest.Expand(Context{
		PreviewID: SanitizePreviewID(env.BeadID),
		Host:      m.cfg.PublicHost,
		Ports:     ports,
	})
	if err != nil {
		return fmt.Errorf("kiln: expanding teardown command for %s: %w", env.BeadID, err)
	}
	penv := PreviewEnv{
		PreviewID:    SanitizePreviewID(env.BeadID),
		BeadID:       env.BeadID,
		Branch:       env.Branch,
		WorktreePath: env.WorktreePath,
		AnvilName:    env.Anvil,
		AnvilPath:    env.AnvilPath,
		Ports:        ports,
	}
	return m.runLifecycle(ctx, "teardown", expanded.Teardown, env, penv)
}

// runLifecycle runs one manifest lifecycle command with the preview's context.
func (m *Manager) runLifecycle(ctx context.Context, name, command string, env *Environment, penv PreviewEnv) error {
	if strings.TrimSpace(command) == "" {
		return nil
	}
	return RunCommand(ctx, CommandSpec{
		Name:         name,
		Command:      command,
		BeadID:       env.BeadID,
		WorktreePath: env.WorktreePath,
		Env:          BuildEnv(m.cfg.Env, penv, nil),
		Timeout:      m.cfg.CommandTimeout,
		Logger:       m.logger,
	})
}

// portMap returns service name → allocated port for a preview. A preview whose
// services never started has no allocation, so the manifest's declared services
// map to 0 and any {{.ServicePort}} in teardown fails loudly rather than
// silently dropping a database name on the floor.
func (m *Manager) portMap(env *Environment) map[string]int {
	ports := make(map[string]int)
	if env.instance != nil {
		for _, svc := range env.instance.Record().Services {
			ports[svc.Name] = svc.Port
		}
	}
	if env.manifest != nil {
		for _, svc := range env.manifest.Services {
			if _, ok := ports[svc.Name]; !ok {
				ports[svc.Name] = 0
			}
		}
	}
	return ports
}

// touchStore bumps last_active_at in the state row, if there is a store.
func (m *Manager) touchStore(beadID string) {
	if m.store == nil {
		return
	}
	if _, err := m.store.TouchPreview(beadID); err != nil {
		m.logger.Warn("kiln: touching preview record failed", "bead", beadID, "error", err)
	}
}

// lockBead acquires the per-bead serializer and returns its release function.
func (m *Manager) lockBead(beadID string) func() {
	m.mu.Lock()
	l, ok := m.locks[beadID]
	if !ok {
		l = &beadLock{}
		m.locks[beadID] = l
	}
	l.refs++
	m.mu.Unlock()

	l.mu.Lock()
	return func() {
		l.mu.Unlock()
		m.mu.Lock()
		l.refs--
		if l.refs == 0 {
			delete(m.locks, beadID)
		}
		m.mu.Unlock()
	}
}

// failureDetail summarizes why every service in a preview failed, for the error
// the operator sees.
func failureDetail(rec state.Preview) string {
	var parts []string
	for _, svc := range rec.Services {
		if svc.Error != "" {
			parts = append(parts, fmt.Sprintf("%s: %s", svc.Name, svc.Error))
			continue
		}
		parts = append(parts, fmt.Sprintf("%s: %s", svc.Name, svc.Health))
	}
	if len(parts) == 0 {
		return "no services started"
	}
	return strings.Join(parts, "; ")
}

// RuntimeRunner adapts a *Runtime to the Runner interface the Manager talks to.
// The indirection exists so the manager's flows can be tested without spawning
// processes; the runtime itself keeps returning its concrete *Preview.
func RuntimeRunner(rt *Runtime) Runner { return runtimeRunner{rt: rt} }

type runtimeRunner struct{ rt *Runtime }

func (r runtimeRunner) Start(ctx context.Context, req StartRequest) (Instance, error) {
	p, err := r.rt.Start(ctx, req)
	if err != nil {
		return nil, err
	}
	return p, nil
}
