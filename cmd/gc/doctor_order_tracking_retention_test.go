package main

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/gastownhall/gascity/internal/beads"
	"github.com/gastownhall/gascity/internal/config"
	"github.com/gastownhall/gascity/internal/doctor"
)

func TestOrderTrackingRetentionCheckRecentVolumeIsNotExpiredBacklog(t *testing.T) {
	rows := makeClosedOrderTrackingBeads(600)
	for i := range rows {
		rows[i].CreatedAt = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)
		rows[i].UpdatedAt = rows[i].CreatedAt
		rows[i].Labels = append(rows[i].Labels, "order-run:health")
	}
	store := beads.NewMemStoreFrom(700, rows, nil)
	check := newOrderTrackingRetentionCheck("/city", func(string) (beads.Store, error) { return store, nil })
	check.now = func() time.Time { return time.Date(2026, 11, 16, 13, 0, 0, 0, time.UTC) }
	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK || !strings.Contains(res.Message, "eligible") {
		t.Fatalf("recent history must not be reported as overdue: %+v", res)
	}
}

func makeClosedOrderTrackingBeads(n int) []beads.Bead {
	out := make([]beads.Bead, n)
	for i := range out {
		out[i] = beads.Bead{
			ID:     fmt.Sprintf("ot-%04d", i),
			Status: "closed",
			Labels: []string{labelOrderTracking, "order-run:health"},
		}
	}
	return out
}

func TestOrderTrackingRetentionCheck_OKWhenBelowThreshold(t *testing.T) {
	store := beads.NewMemStoreFrom(600, makeClosedOrderTrackingBeads(499), nil)
	check := newOrderTrackingRetentionCheck("/city", func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK at 499 beads: %s", res.Status, res.Message)
	}
}

func TestOrderTrackingRetentionCheck_WarningAtThreshold(t *testing.T) {
	store := beads.NewMemStoreFrom(600, makeClosedOrderTrackingBeads(510), nil)
	check := newOrderTrackingRetentionCheck("/city", func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning at 500 beads: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "500") {
		t.Errorf("Message %q missing count 500", res.Message)
	}
}

func TestOrderTrackingRetentionCheck_WarningAboveThreshold(t *testing.T) {
	store := beads.NewMemStoreFrom(700, makeClosedOrderTrackingBeads(600), nil)
	check := newOrderTrackingRetentionCheck("/city", func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning above threshold: %s", res.Status, res.Message)
	}
}

func TestOrderTrackingRetentionCheck_CapFormatAtListLimit(t *testing.T) {
	// 501 closed beads (the list cap): count displays as "≥501", not exact.
	store := beads.NewMemStoreFrom(700, makeClosedOrderTrackingBeads(511), nil)
	check := newOrderTrackingRetentionCheck("/city", func(string) (beads.Store, error) { return store, nil })

	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning at list cap: %s", res.Status, res.Message)
	}
	if !strings.Contains(res.Message, "≥501") {
		t.Fatalf("Message = %q, want ≥501 cap format", res.Message)
	}
}

func TestOrderTrackingRetentionCheck_OKWhenNoStore(t *testing.T) {
	check := newOrderTrackingRetentionCheck("", nil)
	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusOK {
		t.Fatalf("Status = %v, want OK (no store configured means no beads): %s", res.Status, res.Message)
	}
}

func TestOrderTrackingRetentionCheck_WarningOnStoreOpenError(t *testing.T) {
	check := newOrderTrackingRetentionCheck("/city", func(string) (beads.Store, error) {
		return nil, fmt.Errorf("store unreachable")
	})
	res := check.Run(&doctor.CheckContext{})
	if res.Status != doctor.StatusWarning {
		t.Fatalf("Status = %v, want Warning on store open error: %s", res.Status, res.Message)
	}
	if res.Severity != doctor.SeverityAdvisory {
		t.Fatalf("Severity = %v, want Advisory (observability only): %s", res.Severity, res.Message)
	}
}

func TestOrderTrackingRetentionCheck_CheckMetadata(t *testing.T) {
	check := newOrderTrackingRetentionCheck("/city", func(string) (beads.Store, error) {
		return beads.NewMemStore(), nil
	})
	if check.Name() != "order-tracking-retention" {
		t.Errorf("Name() = %q, want order-tracking-retention", check.Name())
	}
	if check.CanFix() {
		t.Error("CanFix() = true, want false (read-only observability check)")
	}
	if check.WarmupEligible() {
		t.Error("WarmupEligible() = true, want false")
	}
}

func TestOrderTrackingRetentionCheck_RegisteredInBuildDoctorChecks(t *testing.T) {
	cityPath := t.TempDir()
	cfg := &config.City{}
	withHealthyStorePreflight(t)
	checks := buildDoctorChecks(cityPath, cfg, nil, buildDoctorChecksOpts{
		SkipCityDoltCheck:    true,
		SkipManagedDoltCheck: true,
	})
	for _, c := range checks {
		if c.Name() == "order-tracking-retention" {
			return
		}
	}
	t.Fatal("order-tracking-retention check not found in buildDoctorChecks output")
}
