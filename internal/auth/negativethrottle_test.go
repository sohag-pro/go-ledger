package auth

import (
	"fmt"
	"testing"
	"time"
)

func TestNegativeThrottle_AllowsUnderBudget(t *testing.T) {
	t.Parallel()

	th := NewNegativeThrottle(3, time.Minute)
	for i := 0; i < 3; i++ {
		if !th.Allowed("1.2.3.4") {
			t.Fatalf("iteration %d: Allowed = false, want true (under budget)", i)
		}
		th.RecordFailure("1.2.3.4")
	}
}

func TestNegativeThrottle_BlocksAfterMaxFailures(t *testing.T) {
	t.Parallel()

	th := NewNegativeThrottle(3, time.Minute)
	for i := 0; i < 3; i++ {
		th.RecordFailure("1.2.3.4")
	}

	if th.Allowed("1.2.3.4") {
		t.Fatal("Allowed = true after maxFailures failures, want false")
	}
}

func TestNegativeThrottle_DifferentIPUnaffected(t *testing.T) {
	t.Parallel()

	th := NewNegativeThrottle(3, time.Minute)
	for i := 0; i < 10; i++ {
		th.RecordFailure("1.2.3.4")
	}

	if !th.Allowed("5.6.7.8") {
		t.Fatal("Allowed(other IP) = false, want true: one IP's flood must not affect another")
	}
}

func TestNegativeThrottle_RecordSuccessResetsBudget(t *testing.T) {
	t.Parallel()

	th := NewNegativeThrottle(3, time.Minute)
	for i := 0; i < 3; i++ {
		th.RecordFailure("1.2.3.4")
	}
	if th.Allowed("1.2.3.4") {
		t.Fatal("Allowed = true before RecordSuccess, want false")
	}

	th.RecordSuccess("1.2.3.4")
	if !th.Allowed("1.2.3.4") {
		t.Fatal("Allowed = false after RecordSuccess, want true (budget reset)")
	}
}

func TestNegativeThrottle_WindowDecay(t *testing.T) {
	t.Parallel()

	th := NewNegativeThrottle(3, time.Minute)
	now := time.Now()
	th.now = func() time.Time { return now }

	for i := 0; i < 3; i++ {
		th.RecordFailure("1.2.3.4")
	}
	if th.Allowed("1.2.3.4") {
		t.Fatal("Allowed = true within the window after maxFailures, want false")
	}

	// Advance past the window: the old entry should be treated as expired.
	now = now.Add(time.Minute + time.Second)
	if !th.Allowed("1.2.3.4") {
		t.Fatal("Allowed = false after the window elapsed, want true (entry decayed)")
	}

	// A failure recorded after the window elapsed starts a fresh window
	// rather than adding to the stale count.
	th.RecordFailure("1.2.3.4")
	if !th.Allowed("1.2.3.4") {
		t.Fatal("Allowed = false after a single failure in a fresh window, want true")
	}
}

func TestNegativeThrottle_MapIsBounded(t *testing.T) {
	t.Parallel()

	th := NewNegativeThrottle(3, time.Minute)
	// Spray failures from more distinct IPs than the tracked cap, all within
	// the same window (no decay to rely on): the map must not grow past the
	// bound no matter how many source IPs an attacker sprays from. The
	// overshoot only needs to be enough to prove the bound holds, not a large
	// multiple: eviction is O(map size) per call once at capacity, and this
	// runs under -race in CI.
	const sprayIPs = negativeThrottleMaxTrackedIPs + 1000
	for i := 0; i < sprayIPs; i++ {
		th.RecordFailure(fmt.Sprintf("10.%d.%d.%d", (i>>16)&0xff, (i>>8)&0xff, i&0xff))
	}

	th.mu.Lock()
	n := len(th.entries)
	th.mu.Unlock()

	if n > negativeThrottleMaxTrackedIPs {
		t.Fatalf("tracked IP count = %d, want <= %d (the throttle's own map must be bounded)", n, negativeThrottleMaxTrackedIPs)
	}
}

func TestNegativeThrottle_DefaultsOnNonPositiveArgs(t *testing.T) {
	t.Parallel()

	th := NewNegativeThrottle(0, 0)
	if th.maxFailures != DefaultNegativeThrottleMaxFailures {
		t.Errorf("maxFailures = %d, want default %d", th.maxFailures, DefaultNegativeThrottleMaxFailures)
	}
	if th.window != DefaultNegativeThrottleWindow {
		t.Errorf("window = %s, want default %s", th.window, DefaultNegativeThrottleWindow)
	}
}

func TestNegativeThrottle_EmptyIPAlwaysAllowedAndIgnoredByRecord(t *testing.T) {
	t.Parallel()

	th := NewNegativeThrottle(1, time.Minute)
	th.RecordFailure("")
	th.RecordFailure("")
	if !th.Allowed("") {
		t.Error("Allowed(\"\") = false, want true: an empty IP must never be blocked globally")
	}

	th.mu.Lock()
	n := len(th.entries)
	th.mu.Unlock()
	if n != 0 {
		t.Errorf("entries for empty IP = %d, want 0 (RecordFailure/RecordSuccess must ignore an empty IP)", n)
	}
}
