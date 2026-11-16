package doctor

import (
	"fmt"
	"sort"

	"github.com/gastownhall/gascity/internal/runtime"
)

// registeredSessionHealth observes persisted names without converting a failed
// registry/runtime read into proof of absence. Minimums apply to pool capacity;
// named sessions carry their own explicit expectation.
func registeredSessionHealth(load SessionInventoryFunc, sp runtime.Provider, ctx *CheckContext) (missing, zombies, unknown []string) {
	inventory, err := load()
	if err != nil {
		return nil, nil, []string{fmt.Sprintf("reading session registry: %v", err)}
	}
	unknown = append(unknown, inventory.Unknown...)
	active := map[string]int{}
	uncertain := map[string]bool{}
	for _, target := range inventory.Sessions {
		if ctx.Canceled() {
			return missing, zombies, append(unknown, "session scan canceled")
		}
		if !target.Expected && !target.CheckHealth {
			continue
		}
		if target.Name == "" {
			unknown = append(unknown, target.Template+": active session has no runtime name")
			uncertain[target.Template] = true
			continue
		}
		live, err := runtime.ObserveLivenessWithError(sp, target.Name, target.ProcessNames)
		if err != nil {
			unknown = append(unknown, fmt.Sprintf("%s: liveness unknown: %v", target.Name, err))
			uncertain[target.Template] = true
			continue
		}
		if target.Expected && !live.Present() {
			missing = append(missing, target.Name)
		}
		if live.Corpse || (live.Running && !live.Alive && len(target.ProcessNames) > 0) {
			zombies = append(zombies, target.Name)
		}
		if live.Alive && target.Pool {
			active[target.Template]++
		}
	}
	for template, minimum := range inventory.Minimums {
		if !uncertain[template] && active[template] < minimum {
			missing = append(missing, fmt.Sprintf("%s: %d active pool sessions; minimum %d", template, active[template], minimum))
		}
	}
	sort.Strings(missing)
	sort.Strings(zombies)
	sort.Strings(unknown)
	return missing, zombies, unknown
}

func registeredSessionResult(name, good, bad string, findings, unknown []string) *CheckResult {
	result := &CheckResult{Name: name, Status: StatusOK, Message: good}
	if len(findings) > 0 || len(unknown) > 0 {
		result.Status = StatusWarning
		result.Message = fmt.Sprintf("%d %s; %d observation(s) incomplete", len(findings), bad, len(unknown))
		result.Details = append(append([]string{}, findings...), unknown...)
	}
	return result
}

func registeredSessionNames(load SessionInventoryFunc) (map[string]bool, error) {
	inventory, err := load()
	names := map[string]bool{}
	for _, target := range inventory.Sessions {
		// Dormant/suspended rows still own their runtime identity.
		if target.Name != "" && !target.Unregistered {
			names[target.Name] = true
		}
	}
	if err != nil {
		return names, fmt.Errorf("reading session registry: %w", err)
	}
	return names, nil
}
