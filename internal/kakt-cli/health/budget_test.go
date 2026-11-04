package health

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type slowCheck struct {
	name string
	pass func() bool
}

func (s *slowCheck) Name() string { return s.name }
func (s *slowCheck) Check(context.Context, string) error {
	if s.pass() {
		return nil
	}
	return errors.New("still converging")
}

// The budget is shared, so a slow early check leaves little for the later ones.
// Reporting the later one as "failed after 1s" sends a reader at the wrong
// component -- which happened: ExternalSecrets took 14m26s of a 15m budget and
// the Applications check was reported failed after one second.
func TestRunningOutOfTimeIsNotReportedAsAFailedCheck(t *testing.T) {
	start := time.Now()
	w := &HealthWaiter{
		Interval: time.Millisecond,
		Timeout:  300 * time.Millisecond,
		Checkers: []HealthChecker{
			// Consumes most of the budget, then passes.
			&slowCheck{"secret stores", func() bool { return time.Since(start) > 250*time.Millisecond }},
			// Never passes, and never gets a fair share.
			&slowCheck{"applications", func() bool { return false }},
		},
		OnCheckStart: func(HealthChecker) {},
		OnCheckPass:  func(HealthChecker) {},
	}

	err := w.Wait(context.Background(), "")
	if err == nil {
		t.Fatal("expected the budget to run out")
	}
	if !strings.Contains(err.Error(), "ran out of time before") {
		t.Errorf("a starved check was reported as a failure:\n  %v", err)
	}
	// And it must say where the time actually went.
	if !strings.Contains(err.Error(), "secret stores") {
		t.Errorf("the report does not name the check that consumed the budget:\n  %v", err)
	}
	if !strings.Contains(err.Error(), "--timeout") {
		t.Errorf("the report does not say what to do about it:\n  %v", err)
	}
}

// A check that had a real share of the budget and still did not pass IS a
// failure, and must keep reading as one.
func TestACheckThatHadItsShareStillFails(t *testing.T) {
	w := &HealthWaiter{
		Interval:     time.Millisecond,
		Timeout:      200 * time.Millisecond,
		Checkers:     []HealthChecker{&slowCheck{"applications", func() bool { return false }}},
		OnCheckStart: func(HealthChecker) {},
		OnCheckPass:  func(HealthChecker) {},
	}
	err := w.Wait(context.Background(), "")
	if err == nil {
		t.Fatal("expected a failure")
	}
	if strings.Contains(err.Error(), "ran out of time before") {
		t.Errorf("a genuinely failing check was excused as a budget problem:\n  %v", err)
	}
	if !strings.Contains(err.Error(), "failed after") {
		t.Errorf("expected a failure report, got:\n  %v", err)
	}
}
