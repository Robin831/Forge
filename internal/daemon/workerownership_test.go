package daemon

import (
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
)

// Every worker row this daemon inserts is stamped with the running generation,
// so the generation cannot tell such a row from a leak — the live-worker
// registry is the ONLY thing standing between it and the reaper, which ends a
// row nothing registers once its heartbeat ages past workerHeartbeatGrace.
// Nothing in the type system pairs an InsertWorker with a trackWorker hold, and
// the one production path that forgot (crucible child pipelines, which let
// pipeline.Run mint an id this daemon never sees) failed in the dangerous
// direction: a live child marked failed, its dispatch slot handed back, a
// spurious worker_leaked event, and no symptom until minute three.
//
// So the pairing is asserted here instead. The test enumerates every
// InsertWorker call site in production code and requires each to be accounted
// for below; a new one fails the test until somebody says how its row is owned.
//
// It is keyed by enclosing function rather than by line so ordinary edits above
// a call site do not churn it.
type insertSite struct {
	// registersInline is true when the enclosing function itself takes the
	// registry hold, which the test then verifies mechanically.
	registersInline bool
	// why records the ownership argument for a site that does not, and cannot,
	// register inline. Purely documentary — but a new call site cannot be
	// admitted without writing one.
	why string
}

var workerInsertSites = map[string]insertSite{
	// The quench/burnish/rebase/assay fix workers: one id for all four
	// actions, registered once at the top of the function.
	"internal/daemon/daemon.go:handleLifecycleAction": {registersInline: true},

	"internal/daemon/daemon.go:handleWardenRerun": {registersInline: true},
	"internal/daemon/daemon.go:handleApproveAsIs": {registersInline: true},
	"internal/daemon/daemon.go:handleForceSmith":  {registersInline: true},

	"internal/daemon/daemon.go:insertPendingWorker": {
		why: "returns the id to the claim path, which hands it to dispatchBead; " +
			"that goroutine registers it (defer d.trackWorker(claimWorkerID)()) " +
			"and owns the row for the whole dispatch, crucible branch included",
	},

	"internal/pipeline/pipeline.go:Run": {
		why: "inserts under Params.WorkerID. The daemon's own dispatch supplies " +
			"the registered claim id; crucible children supply an id " +
			"runChildPipeline mints via pipeline.NewWorkerID and registers " +
			"through Params.TrackWorker before Run is called. A caller that " +
			"leaves WorkerID empty gets a row nothing can register, which is " +
			"the whole reason NewWorkerID is exported",
	},

	"internal/worker/worker.go:New": {
		why: "no production caller — the package predates the pipeline and is " +
			"reachable only from tests. If it is ever wired up, its id has to " +
			"reach a registry the same way the pipeline's does",
	},
}

// pipeline.Run is the one insert site the InsertWorker table above cannot
// police, because the ownership decision is not made there: Run inserts under
// Params.WorkerID and mints an id of its own when that field is empty, so
// whether the row is owned is settled by the CALLER and every caller looks
// identical from inside pipeline.go. Enumerating the insert alone is what let
// runPostForceSmithPipeline insert an unregistered row of the running
// generation with the guard passing — the second production path to make the
// same mistake the crucible child path had just been fixed for.
//
// So Run's callers are enumerated on the same terms, keyed the same way.
var pipelineRunSites = map[string]insertSite{
	"internal/daemon/daemon.go:dispatchBead": {
		why: "runs under the claim id insertPendingWorker returned, which this " +
			"goroutine registers (defer d.trackWorker(claimWorkerID)()) for the " +
			"whole dispatch",
	},

	// Force smith's own worker row is finished before this runs, so the
	// temper → warden → PR phases get an id of their own — minted here
	// precisely so it can be registered before Run inserts the row.
	"internal/daemon/daemon.go:runPostForceSmithPipeline": {registersInline: true},

	"internal/crucible/crucible.go:runChildPipeline": {
		why: "mints the child id with pipeline.NewWorkerID and registers it " +
			"through Params.TrackWorker — the daemon's own registry, injected " +
			"by the crucible params — before Run is called",
	},
}

// repoRoot is the module root, two directories above this package.
func repoRoot(t *testing.T) string {
	t.Helper()
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatalf("resolving repo root: %v", err)
	}
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("expected go.mod at %s: %v", root, err)
	}
	return root
}

// callSites returns "<repo-relative path>:<enclosing func>" for every call to a
// method of the given name in production (non-test) Go files, mapped to the
// source lines the calls sit on.
func callSites(t *testing.T, root, method string) map[string][]int {
	t.Helper()
	found := make(map[string][]int)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			t.Fatalf("relativising %s: %v", path, rerr)
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				se, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok || se.Sel.Name != method {
					return true
				}
				key := rel + ":" + fd.Name.Name
				found[key] = append(found[key], fset.Position(ce.Pos()).Line)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

// qualifiedCallSites is callSites for a call qualified by package name
// (pkg.Fn), which is how the pipeline entry point is reached from every
// package that is not pipeline itself.
func qualifiedCallSites(t *testing.T, root, pkg, fn string) map[string][]int {
	t.Helper()
	found := make(map[string][]int)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			t.Fatalf("relativising %s: %v", path, rerr)
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				ce, ok := n.(*ast.CallExpr)
				if !ok {
					return true
				}
				se, ok := ce.Fun.(*ast.SelectorExpr)
				if !ok || se.Sel.Name != fn {
					return true
				}
				id, ok := se.X.(*ast.Ident)
				if !ok || id.Name != pkg {
					return true
				}
				key := rel + ":" + fd.Name.Name
				found[key] = append(found[key], fset.Position(ce.Pos()).Line)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

// fieldSetSites returns the functions that set the named struct field in a
// composite literal, keyed like callSites. It is what makes the pipeline.Run
// table an assertion rather than a list: a caller that stops passing
// Params.WorkerID hands Run an empty id, which Run fills with one of its own
// that no registry can ever hold — and the call site itself is unchanged, so
// nothing else here would notice.
func fieldSetSites(t *testing.T, root, field string) map[string][]int {
	t.Helper()
	found := make(map[string][]int)
	err := filepath.WalkDir(root, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			switch d.Name() {
			case ".git", "node_modules", "vendor", "web":
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		fset := token.NewFileSet()
		f, perr := parser.ParseFile(fset, path, nil, 0)
		if perr != nil {
			t.Fatalf("parsing %s: %v", path, perr)
		}
		rel, rerr := filepath.Rel(root, path)
		if rerr != nil {
			t.Fatalf("relativising %s: %v", path, rerr)
		}
		rel = filepath.ToSlash(rel)
		for _, decl := range f.Decls {
			fd, ok := decl.(*ast.FuncDecl)
			if !ok || fd.Body == nil {
				continue
			}
			ast.Inspect(fd.Body, func(n ast.Node) bool {
				kv, ok := n.(*ast.KeyValueExpr)
				if !ok {
					return true
				}
				id, ok := kv.Key.(*ast.Ident)
				if !ok || id.Name != field {
					return true
				}
				key := rel + ":" + fd.Name.Name
				found[key] = append(found[key], fset.Position(kv.Pos()).Line)
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walking %s: %v", root, err)
	}
	return found
}

// TestEveryWorkerInsertIsOwned is the guard: no production path may create a
// non-exempt worker row without a registry hold behind it.
func TestEveryWorkerInsertIsOwned(t *testing.T) {
	root := repoRoot(t)

	// The insert that defines the workers table itself is not a call site.
	sites := callSites(t, root, "InsertWorker")
	delete(sites, "internal/state/db.go:InsertWorker")

	for key := range sites {
		if _, ok := workerInsertSites[key]; !ok {
			t.Errorf("unaccounted worker-row insert at %s (lines %v).\n"+
				"Every inserted row carries the running daemon generation, so the "+
				"live-worker registry is the only thing that keeps the reaper from "+
				"ending it (see reaper.go). Register the id with d.trackWorker for "+
				"as long as the row may still be written to, then add the site to "+
				"workerInsertSites in this file — with a reason if the hold is "+
				"taken somewhere else.", key, sites[key])
		}
	}
	for key := range workerInsertSites {
		if _, ok := sites[key]; !ok {
			t.Errorf("workerInsertSites names %s, which no longer inserts a worker row; drop the entry", key)
		}
	}

	checkOwnership(t, root, sites, workerInsertSites, "inserts a worker row")
}

// TestEveryPipelineRunCallerOwnsItsWorkerRow is the same guard one level up:
// pipeline.Run inserts a row for whatever id its caller supplies, and mints one
// nothing can register when the caller supplies none, so the ownership has to
// be asserted where it is decided.
func TestEveryPipelineRunCallerOwnsItsWorkerRow(t *testing.T) {
	root := repoRoot(t)
	sites := qualifiedCallSites(t, root, "pipeline", "Run")

	for key := range sites {
		if _, ok := pipelineRunSites[key]; !ok {
			t.Errorf("unaccounted pipeline.Run call at %s (lines %v).\n"+
				"Run inserts a worker row under Params.WorkerID and mints its own "+
				"id when that field is empty — an id no goroutine can register and "+
				"the reaper therefore ends mid-run. Mint the id with "+
				"pipeline.NewWorkerID, register it (d.trackWorker, or "+
				"Params.TrackWorker across a package boundary), pass it as "+
				"Params.WorkerID, then add the site to pipelineRunSites in this "+
				"file.", key, sites[key])
		}
	}
	for key := range pipelineRunSites {
		if _, ok := sites[key]; !ok {
			t.Errorf("pipelineRunSites names %s, which no longer calls pipeline.Run; drop the entry", key)
		}
	}

	// Every one of them must also hand Run an id, since an id Run mints for
	// itself is exactly the row nothing can register.
	supplied := fieldSetSites(t, root, "WorkerID")
	for key := range sites {
		if _, ok := supplied[key]; !ok {
			t.Errorf("%s calls pipeline.Run without setting Params.WorkerID: Run "+
				"mints an id of its own there, inserts a row of the running daemon "+
				"generation under it, and nothing can register an id it never sees",
				key)
		}
	}

	checkOwnership(t, root, sites, pipelineRunSites, "runs a pipeline")
}

// checkOwnership verifies the half of a table that can be verified: a site
// recorded as registering inline is required to actually call trackWorker, and
// a site that does not register inline is required to say why. Shared by both
// tables so a hold deleted from either kind of call site fails the same way.
func checkOwnership(t *testing.T, root string, sites map[string][]int, table map[string]insertSite, verb string) {
	t.Helper()
	holds := callSites(t, root, "trackWorker")
	for key, site := range table {
		if !site.registersInline {
			if strings.TrimSpace(site.why) == "" {
				t.Errorf("%s neither registers inline nor records why not", key)
			}
			continue
		}
		if _, ok := sites[key]; !ok {
			continue
		}
		if _, ok := holds[key]; !ok {
			t.Errorf("%s %s and is recorded as registering it inline, "+
				"but calls no trackWorker", key, verb)
		}
	}
}

// TestCrucibleChildWorkersAreRegistered pins the wiring the crucible child path
// needs at the daemon end: the params it is handed must carry a TrackWorker, or
// every child row is inserted unowned however carefully the crucible mints it.
func TestCrucibleChildWorkersAreRegistered(t *testing.T) {
	root := repoRoot(t)
	src, err := os.ReadFile(filepath.Join(root, "internal", "daemon", "daemon.go"))
	if err != nil {
		t.Fatalf("reading daemon.go: %v", err)
	}
	if !strings.Contains(string(src), "TrackWorker:") {
		t.Fatal("the crucible params built in daemon.go carry no TrackWorker: " +
			"child pipelines would insert worker rows of the running generation " +
			"that no goroutine registers, and the reaper would end them mid-run")
	}
}

// TestRegistryIsHeldForEveryDaemonInsert is a redundancy check on the table
// above: it reports the set it is asserting so a failure names the whole
// picture rather than one line of it.
func TestRegistryIsHeldForEveryDaemonInsert(t *testing.T) {
	keys := make([]string, 0, len(workerInsertSites))
	for k := range workerInsertSites {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		t.Fatal("workerInsertSites is empty; the guard asserts nothing")
	}
	t.Logf("worker-row insert sites under guard: %s", strings.Join(keys, ", "))
}
