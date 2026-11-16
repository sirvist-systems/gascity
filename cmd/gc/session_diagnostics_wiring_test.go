package main

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/citylayout"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/runtime"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// TestTmuxConfigRuntimeDirReachesDoctorSessionDiagnostics runs the real
// production wiring end to end: the tmux provider config as gc actually builds
// it, through the path the diagnostic writers compose from its RuntimeDir, into
// the directory gc doctor's nudge-unconfirmed check actually scans.
//
// This is the regression test for dr-6siig HIGH finding 1. Both sides had unit
// tests before, and both passed, because each hardcoded its own assumption
// about where the directory is; the writers wrote to <city>/.gc/sessions and
// the check scanned <city>/.gc/runtime/sessions, so a real unconfirmed nudge
// produced a file on disk and a green doctor. Nothing that asserts one side
// against its own assumption can catch that -- only composing the production
// configuration can, which is what this does.
func TestTmuxConfigRuntimeDirReachesDoctorSessionDiagnostics(t *testing.T) {
	city := t.TempDir()

	// Exactly what gc builds for a live session.
	cfg := tmuxConfigFromSession(config.SessionConfig{}, "test-city", city)
	if cfg.RuntimeDir == "" {
		t.Fatal("production tmux config carries no RuntimeDir; diagnostic capture would be disabled fleet-wide")
	}

	// The directory the writers compose from that RuntimeDir, for one session.
	sessionDir := filepath.Join(citylayout.SessionDiagnosticsDirForRuntimeDir(cfg.RuntimeDir), "gc-worker-1")
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatalf("mkdir: %v", err)
	}
	artifact := filepath.Join(sessionDir, "nudge-unconfirmed.log")
	if err := os.WriteFile(artifact, []byte("session: gc-worker-1\n"), 0o600); err != nil {
		t.Fatalf("write diagnostic: %v", err)
	}

	// The check must see it. A StatusOK here is the false green.
	r := doctor.NewNudgeUnconfirmedCheck().Run(&doctor.CheckContext{CityPath: city})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("doctor did not see a diagnostic written through the production runtime dir:\n"+
			"  artifact  = %s\n  status    = %v (%s)\n"+
			"the writers and the check are resolving different directories again", artifact, r.Status, r.Message)
	}
}

// TestDoctorNudgeUnconfirmedCheckSkipsClosedSessions wires the check the way
// buildDoctorChecks does -- live sessions read from the session store -- and
// asserts a diagnostic left by a closed session is not counted while a live
// session's is. Diagnostics are never cleared, so counting closed sessions
// makes the warning grow with the city's age.
func TestDoctorNudgeUnconfirmedCheckSkipsClosedSessions(t *testing.T) {
	city := t.TempDir()
	store := beads.NewMemStore()
	oldOpen := openSessionProviderStore
	t.Cleanup(func() { openSessionProviderStore = oldOpen })
	openSessionProviderStore = func(string) (beads.Store, error) { return store, nil }

	for _, name := range []string{"gc-live-1", "gc-closed-1"} {
		b, err := store.Create(beads.Bead{
			Title:    name,
			Type:     sessionBeadType,
			Labels:   []string{sessionBeadLabel},
			Metadata: map[string]string{"session_name": name, "template": "worker", "state": "awake"},
		})
		if err != nil {
			t.Fatalf("create session bead %s: %v", name, err)
		}
		if name == "gc-closed-1" {
			if err := store.Close(b.ID); err != nil {
				t.Fatalf("close session bead %s: %v", name, err)
			}
		}
		dir := filepath.Join(citylayout.SessionDiagnosticsDir(city), name)
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "startup-nudge-unconfirmed.log"), []byte("session: "+name+"\n"), 0o600); err != nil {
			t.Fatalf("write diagnostic: %v", err)
		}
	}

	check := doctor.NewNudgeUnconfirmedCheck(doctor.WithLiveSessionNames(doctorLiveSessionNames(city, nil)))
	r := check.Run(&doctor.CheckContext{CityPath: city})
	if r.Status != doctor.StatusWarning {
		t.Fatalf("status = %v (%s), want a warning for the live session", r.Status, r.Message)
	}
	if len(r.Details) != 1 || !strings.Contains(r.Details[0], "gc-live-1") {
		t.Fatalf("details = %v, want only gc-live-1", r.Details)
	}
	if !strings.Contains(r.Message, "1 closed session(s) skipped") {
		t.Fatalf("message = %q, want it to report the skipped closed session", r.Message)
	}
}

func TestDoctorSessionInventoryRequiredAndHeldRows(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for _, scenario := range []string{"active missing", "demand asleep", "always asleep", "always absent", "always drained", "always kill pending", "expired quarantine", "historical suspension", "operator hold", "future quarantine", "unreadable hold", "rig suspended", "rig resumed", "legacy pool"} {
		t.Run(scenario, func(t *testing.T) {
			cfg := &config.City{Workspace: config.Workspace{Name: "demo"}, Agents: []config.Agent{{Name: "worker", Dir: "rig", ProcessNames: []string{"claude"}}}, Rigs: []config.Rig{{Name: "rig", Path: "/city/rig"}}}
			row := session.Info{ID: "row", Type: session.BeadType, Template: "rig/worker", State: session.StateActive, MetadataState: "active", SessionName: "dynamic", PoolManaged: true}
			state := suspensionstate.State{}
			wantWarning := true
			var rows []session.Info
			switch scenario {
			case "demand asleep":
				row.State = session.StateAsleep
				row.MetadataState = "asleep"
				wantWarning = false
			case "always asleep", "always absent", "always drained", "always kill pending":
				cfg.NamedSessions = []config.NamedSession{{Name: "always", Template: "worker", Dir: "rig", Mode: "always"}}
				row.State = session.StateAsleep
				row.MetadataState = "asleep"
				row.ConfiguredNamedSession = true
				row.ConfiguredNamedIdentity = "rig/always"
				row.Alias = "rig/always"
				row.PoolManaged = false
				if scenario == "always kill pending" {
					row.StateReason = "kill-pending"
					row.SleepReason = "killed"
					row.SleptAt = now.Add(-time.Second).Format(time.RFC3339)
					wantWarning = false
				}
				if scenario == "always drained" {
					row.MetadataState = "drained"
					wantWarning = false
				}
			case "expired quarantine":
				row.QuarantinedUntil = now.Add(-time.Hour).Format(time.RFC3339)
			case "historical suspension":
				row.SuspendedAt = now.Add(-time.Hour).Format(time.RFC3339)
			case "operator hold":
				row.SleepIntent = "user-hold"
				wantWarning = false
			case "future quarantine":
				row.QuarantinedUntil = now.Add(time.Hour).Format(time.RFC3339)
				wantWarning = false
			case "unreadable hold":
				row.HeldUntil = "invalid"
			case "rig suspended":
				value := true
				state.Rigs = map[string]suspensionstate.Override{"rig": {Suspended: &value}}
				wantWarning = false
			case "rig resumed":
				value := false
				cfg.Rigs[0].Suspended = true
				state.Rigs = map[string]suspensionstate.Override{"rig": {Suspended: &value}}
			case "legacy pool":
				row.PoolManaged = false
				row.PoolSlot = "1"
				minimum := 1
				cfg.Agents[0].MinActiveSessions = &minimum
				wantWarning = false
			}
			if scenario != "always absent" {
				rows = []session.Info{row}
			}
			inventory := doctorSessionInventoryFromInfos("/city", cfg, state, rows, now)
			sp := runtime.NewFake()
			if scenario == "legacy pool" {
				if err := sp.Start(context.Background(), row.SessionName, runtime.Config{}); err != nil {
					t.Fatal(err)
				}
			}
			check := doctor.NewAgentSessionsCheck(cfg, "demo", "", sp)
			check.Sessions = func() (doctor.SessionInventory, error) { return inventory, nil }
			result := check.Run(&doctor.CheckContext{})
			if scenario == "always asleep" && strings.Join(result.Details, " ") != "dynamic" {
				t.Fatalf("canonical registered row was not tested: %+v", result)
			}
			if (result.Status == doctor.StatusWarning) != wantWarning {
				t.Fatalf("unexpected runtime requirement: %+v; inventory=%+v", result, inventory)
			}
		})
	}
}

func TestDoctorSessionInventoryReadsTypeOnlyAndLegacyOwners(t *testing.T) {
	city := t.TempDir()
	primary := beads.NewMemStore()
	legacy := beads.NewMemStore()
	old := openSessionProviderStore
	t.Cleanup(func() { openSessionProviderStore = old })
	openSessionProviderStore = func(string) (beads.Store, error) { return primary, nil }
	cfg := &config.City{Agents: []config.Agent{{Name: "worker"}}, Rigs: []config.Rig{{Name: "rig", Path: filepath.Join(city, "rig")}}}
	for name, store := range map[string]*beads.MemStore{"type-only": primary, "legacy": legacy} {
		if _, err := store.Create(beads.Bead{Type: sessionBeadType, Title: name, Metadata: map[string]string{"session_name": name, "template": "worker", "state": "awake"}}); err != nil {
			t.Fatal(err)
		}
	}
	sp := runtime.NewFake()
	for _, name := range []string{"type-only", "legacy"} {
		if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
			t.Fatal(err)
		}
	}
	scopes := newOwnerlessRuntimesCheck(cfg, city, func(path string) (beads.Store, error) {
		if path == cfg.Rigs[0].Path {
			return legacy, nil
		}
		return primary, nil
	}, sp, nil)
	check := doctor.NewOrphanSessionsCheck(cfg, "demo", "", sp)
	check.Sessions = doctorOwnershipInventory(city, cfg, scopes)
	ctx := &doctor.CheckContext{CityPath: city}
	if result := check.Run(ctx); result.Status != doctor.StatusOK {
		t.Fatalf("valid owner lost: %+v", result)
	}
	if err := check.Fix(ctx); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"type-only", "legacy"} {
		if !sp.IsRunning(name) {
			t.Fatalf("registered runtime stopped: %s", name)
		}
	}
	scopes.newStore = func(path string) (beads.Store, error) {
		if path == cfg.Rigs[0].Path {
			return nil, errDoctorStoreSuspended
		}
		return primary, nil
	}
	if result := check.Run(ctx); result.Status != doctor.StatusWarning || !strings.Contains(result.Message, "not fully checked") {
		t.Fatalf("incomplete ownership hidden: %+v", result)
	}
	if err := check.Fix(ctx); err == nil {
		t.Fatal("suspended ownership leg permitted cleanup")
	}
	if !sp.IsRunning("legacy") {
		t.Fatal("runtime owned by suspended rig stopped")
	}
}

func TestDoctorRoutingRuntimeSuspensionDoesNotOpenHeldStore(t *testing.T) {
	city := t.TempDir()
	held := true
	if err := suspensionstate.Save(fsys.OSFS{}, city, suspensionstate.State{Rigs: map[string]suspensionstate.Override{"rig": {Suspended: &held}}}); err != nil {
		t.Fatal(err)
	}
	cfg := &config.City{Rigs: []config.Rig{{Name: "rig", Path: filepath.Join(city, "rig")}}}
	factory := func(path string) (beads.Store, error) {
		if path != city {
			t.Fatalf("opened held rig: %s", path)
		}
		return beads.NewMemStore(), nil
	}
	checks := []doctor.Check{newRunTargetRoutedToBackfillCheck(cfg, city, factory), newRouteRecoveryQuarantineCheck(cfg, city, factory), newHoldLabelRoutedToCheck(cfg, city, factory)}
	for _, check := range checks {
		result := check.Run(&doctor.CheckContext{CityPath: city})
		if result.Status != doctor.StatusOK || !strings.Contains(strings.Join(result.Details, " "), "suspended") {
			t.Fatalf("%s: %+v", check.Name(), result)
		}
		if err := check.Fix(&doctor.CheckContext{CityPath: city}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestDoctorSessionChecksFollowRunningMode(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname=\"demo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")
	old := doctorControllerSessionMode
	t.Cleanup(func() { doctorControllerSessionMode = old })
	for _, mode := range []reconcilerMode{reconcilerLegacy, reconcilerV2} {
		doctorControllerSessionMode = func(string, time.Duration) (reconcilerMode, bool, error) { return mode, true, nil }
		cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
		cfg.Daemon.SessionReconciler = "v2"
		if mode == reconcilerV2 {
			cfg.Daemon.SessionReconciler = "legacy"
		}
		checks := buildDoctorChecks(city, cfg, nil, buildDoctorChecksOpts{ControllerRunning: true, SkipCityDoltCheck: true, SkipManagedDoltCheck: true})
		found := false
		for _, check := range checks {
			if sessions, ok := check.(*doctor.AgentSessionsCheck); ok {
				found = true
				if (sessions.Sessions != nil) != (mode == reconcilerV2) {
					t.Fatalf("checks followed disk instead of running mode %s", mode)
				}
			}
		}
		if !found {
			t.Fatal("missing agent-sessions wiring")
		}
	}
}

func TestDoctorConfiguredNameWithoutRowIsNotRegisteredOwnership(t *testing.T) {
	city := t.TempDir()
	primary := beads.NewMemStore()
	old := openSessionProviderStore
	t.Cleanup(func() { openSessionProviderStore = old })
	openSessionProviderStore = func(string) (beads.Store, error) { return primary, nil }
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}, Agents: []config.Agent{{Name: "worker"}}, NamedSessions: []config.NamedSession{{Name: "named", Template: "worker"}}, Rigs: []config.Rig{{Name: "held", Path: filepath.Join(city, "held")}}}
	sp := runtime.NewFake()
	name := config.NamedSessionRuntimeName("demo", cfg.Workspace, "named")
	if err := sp.Start(context.Background(), name, runtime.Config{}); err != nil {
		t.Fatal(err)
	}
	for _, suspended := range []bool{false, true} {
		scopes := newOwnerlessRuntimesCheck(cfg, city, func(path string) (beads.Store, error) {
			if suspended && path == cfg.Rigs[0].Path {
				return nil, errDoctorStoreSuspended
			}
			return primary, nil
		}, sp, nil)
		check := doctor.NewOrphanSessionsCheck(cfg, "demo", "", sp)
		check.Sessions = doctorOwnershipInventory(city, cfg, scopes)
		result := check.Run(&doctor.CheckContext{CityPath: city})
		if result.Status != doctor.StatusWarning {
			t.Fatalf("configured name was mistaken for persisted ownership: %+v", result)
		}
		if suspended && !strings.Contains(result.Message, "not fully checked") {
			t.Fatalf("incomplete ownership hidden: %+v", result)
		}
	}
}

func TestDoctorSessionChecksReportUnknownRunningMode(t *testing.T) {
	city := t.TempDir()
	if err := os.WriteFile(filepath.Join(city, "city.toml"), []byte("[workspace]\nname=\"demo\"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GC_DOLT", "skip")
	old := doctorControllerSessionMode
	t.Cleanup(func() { doctorControllerSessionMode = old })
	doctorControllerSessionMode = func(string, time.Duration) (reconcilerMode, bool, error) { return reconcilerV2, false, nil }
	cfg := &config.City{Workspace: config.Workspace{Name: "demo"}}
	for _, check := range buildDoctorChecks(city, cfg, nil, buildDoctorChecksOpts{ControllerRunning: true, SkipCityDoltCheck: true, SkipManagedDoltCheck: true}) {
		if check.Name() == "agent-sessions" {
			result := check.Run(&doctor.CheckContext{CityPath: city})
			if result.Status != doctor.StatusWarning || !strings.Contains(strings.Join(result.Details, " "), "mode unknown") {
				t.Fatalf("unknown running mode hidden: %+v", result)
			}
			return
		}
	}
	t.Fatal("missing agent-sessions check")
}

func TestDoctorSessionStartupIsNotAZombie(t *testing.T) {
	now := time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
	for _, state := range []session.State{session.StateCreating, session.StateStartPending, session.StateFailedCreate, session.StateActive, "awake"} {
		t.Run(string(state), func(t *testing.T) {
			cfg := &config.City{Agents: []config.Agent{{Name: "worker", ProcessNames: []string{"claude"}}}}
			info := session.Info{ID: "creating-row", Type: session.BeadType, Template: "worker", SessionName: "starting", State: state, MetadataState: string(state), PendingCreateClaim: state == session.StateActive || state == "awake"}
			inventory := doctorSessionInventoryFromInfos("/city", cfg, suspensionstate.State{}, []session.Info{info}, now)
			sp := runtime.NewFake()
			if err := sp.Start(context.Background(), "starting", runtime.Config{}); err != nil {
				t.Fatal(err)
			}
			sp.Zombies["starting"] = true // shell exists; provider has not launched the agent yet
			check := doctor.NewZombieSessionsCheck(cfg, "demo", "", sp)
			check.Sessions = func() (doctor.SessionInventory, error) { return inventory, nil }
			ctx := &doctor.CheckContext{CityPath: t.TempDir()}
			if result := check.Run(ctx); result.Status != doctor.StatusOK {
				t.Fatalf("normal startup reported dead: %+v", result)
			}
			if err := check.Fix(ctx); err != nil {
				t.Fatal(err)
			}
			if !sp.IsRunning("starting") {
				t.Fatal("doctor stopped an unfinished provider Start")
			}
		})
	}
}
