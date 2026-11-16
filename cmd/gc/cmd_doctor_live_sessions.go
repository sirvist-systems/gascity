package main

import (
	"errors"
	"fmt"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
	"github.com/gastownhall/gascity/internal/fsys"
	"github.com/gastownhall/gascity/internal/session"
	"github.com/gastownhall/gascity/internal/suspensionstate"
)

// doctorLiveSessionInfos lists every live (non-closed) session from the
// session store. Doctor checks that scope themselves to live sessions share it
// so they agree on what live means.
func doctorLiveSessionInfos(cityPath string, cfg *config.City) ([]session.Info, error) {
	store, err := openSessionProviderStore(cityPath)
	if err != nil {
		return nil, err
	}
	infos, err := session.NewStore(beads.SessionStore{Store: cliSessionStore(store, cfg, cityPath)}).ListLabeledSessionInfosUnfiltered()
	if err != nil {
		return nil, err
	}
	live := infos[:0]
	for _, info := range infos {
		if !info.Closed {
			live = append(live, info)
		}
	}
	return live, nil
}

// doctorLiveSessionNames returns a lazy lookup of the tmux session name of
// every live session, for the nudge-unconfirmed check: the diagnostic files it
// scans live under a directory named after the session, and are never cleared
// when the session closes. Laziness keeps the session store out of doctor
// check construction; a store failure is returned so the check can say it
// could not filter instead of reporting a city with no live sessions.
func doctorLiveSessionNames(cityPath string, cfg *config.City) doctor.LiveSessionNamesFunc {
	return func() (map[string]struct{}, error) {
		infos, err := doctorLiveSessionInfos(cityPath, cfg)
		if err != nil {
			return nil, err
		}
		names := make(map[string]struct{}, len(infos))
		for _, info := range infos {
			if info.SessionName != "" {
				names[info.SessionName] = struct{}{}
			}
		}
		return names, nil
	}
}

// doctorSessionInventory reads persisted identities, never the live catalog
// overlay (which can hide a missing active runtime by calling the row asleep).
func doctorSessionInventory(cityPath string, cfg *config.City) doctor.SessionInventoryFunc {
	return func() (doctor.SessionInventory, error) {
		store, err := openSessionProviderStore(cityPath)
		if err != nil {
			return doctor.SessionInventory{}, err
		}
		infos, err := cliSessionFrontDoor(store, cfg, cityPath).ListAll(session.ListAllOptions{Live: true, TierMode: beads.TierBoth})
		if err != nil {
			return doctor.SessionInventory{}, err
		}
		state, err := loadSuspensionState(fsys.OSFS{}, cityPath)
		if err != nil {
			return doctor.SessionInventory{}, err
		}
		return doctorSessionInventoryFromInfos(cityPath, cfg, state, infos, time.Now()), nil
	}
}

func doctorSessionInventoryFromInfos(cityPath string, cfg *config.City, state suspensionstate.State, infos []session.Info, now time.Time) doctor.SessionInventory {
	inventory := doctor.SessionInventory{Minimums: map[string]int{}}
	indices := map[string]int{}
	for _, info := range infos {
		if info.Closed {
			continue
		}
		a := config.FindAgent(cfg, info.Template)
		disposition := session.DispositionOfInfo(info)
		if disposition.Unknown != 0 {
			inventory.Unknown = append(inventory.Unknown, fmt.Sprintf("%s: unreadable session hold facts (%d)", info.ID, disposition.Unknown))
		}
		held := disposition.HoldsDeliberately(now) || session.IsKillPendingInfo(info, now) || isAgentEffectivelySuspendedWith(cfg, cityPath, a, state)
		expected := info.State == session.StateActive || info.State == "awake" || info.State == ""
		// A provider's runtime shell can precede the agent process. Even an
		// awake row can still belong to an unfinished provider Start (#6104).
		healthState := expected || info.State == session.StateAsleep || info.State == session.StateDrained
		expected = expected && !held && !info.PendingCreateClaim
		target := doctor.SessionExpectation{Name: info.SessionName, Template: info.Template, Expected: expected, CheckHealth: healthState && !held && !info.PendingCreateClaim, Pool: isPoolManagedSessionInfo(info) && !session.IsNamedSessionInfo(info)}
		if a != nil {
			target.ProcessNames = a.ProcessNames
		}
		indices[info.ID] = len(inventory.Sessions)
		inventory.Sessions = append(inventory.Sessions, target)
	}
	for i := range cfg.Agents {
		a := &cfg.Agents[i]
		if !isAgentEffectivelySuspendedWith(cfg, cityPath, a, state) && a.EffectiveMinActiveSessions() > 0 {
			inventory.Minimums[a.QualifiedName()] = a.EffectiveMinActiveSessions()
		}
	}
	for _, named := range cfg.NamedSessions {
		spec, ok := session.FindNamedSessionSpec(cfg, loadedCityName(cfg, cityPath), named.QualifiedName())
		if !ok {
			continue
		} // the config check owns disabled/unresolved templates
		row, registered := session.FindCanonicalNamedSessionInfo(infos, spec)
		if registered {
			if index, exists := indices[row.ID]; exists && spec.Mode == "always" && row.MetadataState != string(session.StateDrained) && (row.State == session.StateAsleep || row.State == session.StateActive || row.State == "") && inventory.Sessions[index].CheckHealth {
				inventory.Sessions[index].Expected = true
			}
			continue
		}
		expected := spec.Mode == "always" && !isAgentEffectivelySuspendedWith(cfg, cityPath, spec.Agent, state)
		inventory.Sessions = append(inventory.Sessions, doctor.SessionExpectation{Name: spec.SessionName, Template: spec.Agent.QualifiedName(), Expected: expected, ProcessNames: spec.Agent.ProcessNames, Unregistered: true})
	}
	return inventory
}

// doctorOwnershipInventory adds historical session legs to the primary binding.
// A suspended or failed leg returns partial positive ownership plus an error;
// callers may recognize existing owners but cannot prove any runtime orphaned.
func doctorOwnershipInventory(cityPath string, cfg *config.City, scopes *ownerlessRuntimesCheck) doctor.SessionInventoryFunc {
	return func() (doctor.SessionInventory, error) {
		inventory, err := doctorSessionInventory(cityPath, cfg)()
		if err != nil {
			return inventory, err
		}
		city, err := scopes.newStore(cityPath)
		if err != nil {
			return inventory, err
		}
		owners := scopes.ownerLookup(city)
		if owners.err != nil {
			return inventory, owners.err
		}
		var errs []error
		for _, store := range owners.stores {
			infos, err := sessionFrontDoor(store).ListAll(session.ListAllOptions{Live: true, TierMode: beads.TierBoth})
			if err != nil {
				errs = append(errs, fmt.Errorf("session ownership: %w", err))
				continue
			}
			for _, info := range infos {
				if !info.Closed {
					inventory.Sessions = append(inventory.Sessions, doctor.SessionExpectation{Name: info.SessionName})
				}
			}
		}
		return inventory, errors.Join(errs...)
	}
}
