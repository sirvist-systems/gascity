package main

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"github.com/gastownhall/gascity/internal/beadmeta"
	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

// holdLabelExternalValue is a hold:<value> value that never implies a routing
// gap: it names a human/out-of-system dependency, not an agent.
const holdLabelExternalValue = "external"

// holdLabelClearedValue is the other: `bd set-state <id> hold=cleared` releases
// a hold and leaves hold:cleared behind, so it means "no hold", not an agent.
const holdLabelClearedValue = "cleared"

// holdLabelRoutedToCheck detects beads carrying a hold:<value> label whose
// gc.routed_to metadata is missing or does not match <value>. gc.routed_to is
// the sole persisted routing key (ga-eld2x); a hold:<value> label with no
// matching gc.routed_to has silently drifted from its intended route.
// --fix backfills gc.routed_to from the label value.
type holdLabelRoutedToCheck struct {
	cfg      *config.City
	cityPath string
	newStore func(string) (beads.Store, error)
}

func newHoldLabelRoutedToCheck(cfg *config.City, cityPath string, newStore func(string) (beads.Store, error)) *holdLabelRoutedToCheck {
	return &holdLabelRoutedToCheck{cfg: cfg, cityPath: cityPath, newStore: newStore}
}

func (c *holdLabelRoutedToCheck) Name() string { return "hold-label-routed-to" }

func (c *holdLabelRoutedToCheck) CanFix() bool { return true }

func (c *holdLabelRoutedToCheck) WarmupEligible() bool { return false }

// holdLabelValue returns the hold value carried by labels, if any
// hold:<value> label is present and <value> is not "external" or "cleared".
func holdLabelValue(labels []string) (string, bool) {
	for _, l := range labels {
		val, ok := strings.CutPrefix(l, "hold:")
		if !ok {
			continue
		}
		val = strings.TrimSpace(val)
		if val == "" || val == holdLabelExternalValue || val == holdLabelClearedValue {
			continue
		}
		return val, true
	}
	return "", false
}

// holdRouteTarget is a single bead whose hold:<value> label and gc.routed_to
// metadata have drifted apart.
type holdRouteTarget struct {
	label  string
	store  beads.Store
	beadID string
	want   string
	got    string
}

func (c *holdLabelRoutedToCheck) collect() (targets []holdRouteTarget, skipped, excluded []string) {
	scopes, excluded, scopeErr := doctorActiveScopes(c.cfg, c.cityPath)
	if scopeErr != nil {
		return nil, []string{scopeErr.Error()}, excluded
	}
	for _, sc := range scopes {
		if c.newStore == nil || strings.TrimSpace(sc.path) == "" {
			continue
		}
		store, err := c.newStore(sc.path)
		if err != nil {
			if errors.Is(err, errDoctorStoreSuspended) {
				excluded = append(excluded, sc.label+": not checked: suspended")
				continue
			}
			skipped = append(skipped, fmt.Sprintf("%s skipped: opening bead store: %v", sc.label, err))
			continue
		}
		// hold:<value> carries a dynamic value suffix, so no targeted
		// label/metadata query is possible; AllowScan is required for a
		// broad filter (internal/beads/query.go). Status is left unset so the
		// scan matches every non-closed bead (open, in_progress, blocked,
		// deferred, ...), not just "open" — an exact Status match would
		// silently hide hold:<value> drift on any other status (ga-fm2vgd.2).
		items, err := store.List(beads.ListQuery{AllowScan: true})
		if err != nil {
			skipped = append(skipped, fmt.Sprintf("%s skipped: listing beads: %v", sc.label, err))
			continue
		}
		for _, b := range items {
			want, ok := holdLabelValue(b.Labels)
			if !ok {
				continue
			}
			got := strings.TrimSpace(b.Metadata[beadmeta.RoutedToMetadataKey])
			if got == want {
				continue
			}
			targets = append(targets, holdRouteTarget{label: sc.label, store: store, beadID: b.ID, want: want, got: got})
		}
	}
	return targets, skipped, excluded
}

func (c *holdLabelRoutedToCheck) Run(_ *doctor.CheckContext) *doctor.CheckResult {
	targets, skipped, excluded := c.collect()
	if len(targets) == 0 && len(skipped) == 0 {
		result := okCheck(c.Name(), "no hold:<value> labels are missing a matching gc.routed_to in checked scopes")
		result.Details = excluded
		return result
	}
	details := make([]string, 0, len(targets)+len(skipped))
	for _, tgt := range targets {
		details = append(details, fmt.Sprintf("%s bead %s has hold:%s but gc.routed_to=%q", tgt.label, tgt.beadID, tgt.want, tgt.got))
	}
	details = append(details, skipped...)
	details = append(details, excluded...)
	sort.Strings(details)
	if len(targets) == 0 {
		return warnCheck(c.Name(),
			fmt.Sprintf("hold-label-routed-to check skipped %d scope(s)", len(skipped)),
			"fix bead store access, then rerun gc doctor",
			details)
	}
	return warnCheck(c.Name(),
		fmt.Sprintf("%d bead(s) carry a hold:<value> label without matching gc.routed_to", len(targets)),
		"run gc doctor --fix to backfill gc.routed_to from the hold:<value> label",
		details)
}

func (c *holdLabelRoutedToCheck) Fix(_ *doctor.CheckContext) error {
	targets, skipped, _ := c.collect()
	for _, tgt := range targets {
		if err := tgt.store.SetMetadata(tgt.beadID, beadmeta.RoutedToMetadataKey, tgt.want); err != nil {
			return fmt.Errorf("%s bead %s: backfill gc.routed_to: %w", tgt.label, tgt.beadID, err)
		}
	}
	if len(skipped) > 0 {
		return fmt.Errorf("hold-label-routed-to skipped %d scope(s): %s", len(skipped), strings.Join(skipped, "; "))
	}
	return nil
}
