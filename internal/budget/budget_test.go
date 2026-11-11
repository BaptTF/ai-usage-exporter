package budget

import (
	"math"
	"testing"
	"time"
)

// Fixtures captured from the legacy dashboard on 2026-08-25T22:47:54Z. They
// pin down the two period rules the real API follows: monthly resets on the
// subscription anniversary (arbitrary day and time, one per key), weekly resets
// on a calendar Monday 00:00 UTC shared by all keys.
var captureNow = mustTime("2026-08-25T22:47:54.286Z")

func mustTime(s string) time.Time {
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil {
		panic(err)
	}
	return t
}

func monthly(pct int, resetsAt string) Window {
	return Window{Name: "Monthly", Kind: KindMonthly, Percent: pct, ResetsAt: mustTime(resetsAt)}
}

func closeTo(t *testing.T, label string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Errorf("%s = %.4f, want %.4f (±%.g)", label, got, want, tol)
	}
}

func TestComputeBudgetRealCapture(t *testing.T) {
	cases := []struct {
		label         string
		window        Window
		wantElapsed   int
		wantAhead     int
		wantProjected int
		wantDryDays   float64
		wantLevel     Level
	}{
		// 28% burned in under 4 days: the wall lands 17 days before the reset.
		{"Main", monthly(28, "2026-09-22T01:23:30.224Z"), 13, 15, 223, 17.100, LevelRed},
		{"N", monthly(28, "2026-09-21T21:57:17.220Z"), 13, 15, 215, 16.589, LevelRed},
		// Marginally ahead of budget: a couple of dry days, not a derailment.
		{"A", monthly(77, "2026-09-03T21:29:30.223Z"), 71, 6, 108, 2.360, LevelAmber},
		{"R", monthly(80, "2026-09-02T05:19:30.222Z"), 77, 3, 105, 1.340, LevelAmber},
	}

	for _, tc := range cases {
		t.Run(tc.label, func(t *testing.T) {
			b := ComputeBudget(tc.window, captureNow)

			if !b.Valid {
				t.Fatal("Valid = false, want true for a monthly window")
			}
			closeTo(t, "PeriodDays", b.PeriodDays, 31, 0.001)
			if b.ElapsedPct != tc.wantElapsed {
				t.Errorf("ElapsedPct = %d, want %d", b.ElapsedPct, tc.wantElapsed)
			}
			if b.AheadPct != tc.wantAhead {
				t.Errorf("AheadPct = %d, want %d", b.AheadPct, tc.wantAhead)
			}
			if b.ProjectedPct != tc.wantProjected {
				t.Errorf("ProjectedPct = %d, want %d", b.ProjectedPct, tc.wantProjected)
			}
			closeTo(t, "DryDays", b.DryDays, tc.wantDryDays, 0.01)
			if b.Level != tc.wantLevel {
				t.Errorf("Level = %q, want %q", b.Level, tc.wantLevel)
			}
			if b.EarlyPeriod {
				t.Error("EarlyPeriod = true, want false (all captured periods are past 5%)")
			}
		})
	}
}

// The captured levels must actually discriminate: a flat run of four reds
// would convey nothing, which is why the red threshold sits at 3 dry days.
func TestCaptureLevelsAreDiscriminating(t *testing.T) {
	windows := []Window{
		monthly(28, "2026-09-22T01:23:30.224Z"),
		monthly(28, "2026-09-21T21:57:17.220Z"),
		monthly(77, "2026-09-03T21:29:30.223Z"),
		monthly(80, "2026-09-02T05:19:30.222Z"),
	}

	counts := map[Level]int{}
	for _, w := range windows {
		counts[ComputeBudget(w, captureNow).Level]++
	}

	if counts[LevelRed] != 2 || counts[LevelAmber] != 2 {
		t.Errorf("level distribution = %v, want 2 red and 2 amber", counts)
	}
}

func TestWeeklyIsCalendarAligned(t *testing.T) {
	w := Window{Name: "Weekly", Kind: KindWeekly, Percent: 38, ResetsAt: mustTime("2026-08-31T00:00:00.224Z")}
	b := ComputeBudget(w, captureNow)

	if !b.Valid {
		t.Fatal("Valid = false, want true")
	}
	closeTo(t, "PeriodDays", b.PeriodDays, 7, 0.001)

	// Monday 24 August, not an anniversary a month back.
	if got, want := b.PeriodStart.UTC(), mustTime("2026-08-24T00:00:00.224Z"); !got.Equal(want) {
		t.Errorf("PeriodStart = %s, want %s", got, want)
	}
	if b.PeriodStart.UTC().Weekday() != time.Monday {
		t.Errorf("PeriodStart weekday = %s, want Monday", b.PeriodStart.UTC().Weekday())
	}
	closeTo(t, "DryDays", b.DryDays, 1.869, 0.01)
	if b.Level != LevelAmber {
		t.Errorf("Level = %q, want amber", b.Level)
	}
}

func TestDailyIsTwentyFourHours(t *testing.T) {
	now := mustTime("2026-09-21T12:00:00Z")
	w := Window{Name: "Daily", Kind: KindDaily, Percent: 50, ResetsAt: mustTime("2026-09-22T00:00:00Z")}
	b := ComputeBudget(w, now)

	if !b.Valid {
		t.Fatal("Valid = false, want true for a daily window")
	}
	closeTo(t, "PeriodDays", b.PeriodDays, 1, 0.0001)
	if b.ElapsedPct != 50 {
		t.Errorf("ElapsedPct = %d, want 50", b.ElapsedPct)
	}
	if b.Level != LevelGreen {
		t.Errorf("Level = %q, want green (on pace)", b.Level)
	}
}

// With zero usage the API returns resetsAt = now+5h, so a derived period start
// would sit at "now" and make the rate infinite. No pace maths on rolling.
func TestRollingHasNoBudget(t *testing.T) {
	w := Window{Name: "Rolling 5h", Kind: KindRolling, Percent: 0, ResetsAt: captureNow.Add(5 * time.Hour)}
	b := ComputeBudget(w, captureNow)

	if b.Valid {
		t.Error("Valid = true, want false for the rolling window")
	}
	if b.RatePerDay != 0 || b.DryDays != 0 || !b.WallAt.IsZero() {
		t.Errorf("expected no pace maths, got rate=%v dry=%v wall=%v", b.RatePerDay, b.DryDays, b.WallAt)
	}
}

func TestMonthlyStartKeepsAnniversary(t *testing.T) {
	cases := []struct {
		reset string
		want  string
	}{
		{"2026-09-22T01:23:30Z", "2026-08-22T01:23:30Z"},
		{"2026-09-02T05:19:30Z", "2026-08-02T05:19:30Z"},
		{"2026-01-15T12:00:00Z", "2025-12-15T12:00:00Z"}, // across the year boundary
		// A 31st anniversary has no counterpart in February: clamp to the last
		// day of the month instead of letting Go roll forward into March.
		{"2026-03-31T08:00:00Z", "2026-02-28T08:00:00Z"},
		{"2028-03-31T08:00:00Z", "2028-02-29T08:00:00Z"}, // leap year
		{"2026-05-31T08:00:00Z", "2026-04-30T08:00:00Z"},
	}

	for _, tc := range cases {
		got := monthlyStart(mustTime(tc.reset))
		if want := mustTime(tc.want); !got.Equal(want) {
			t.Errorf("monthlyStart(%s) = %s, want %s", tc.reset, got.UTC(), want)
		}
	}
}

// One heavy session on day one projects to a wild overshoot. That must not
// paint the dashboard red before the average rate means anything.
func TestEarlyPeriodCapsPaceDrivenRed(t *testing.T) {
	reset := mustTime("2026-09-22T00:00:00Z")
	now := mustTime("2026-08-23T00:00:00Z") // day 1 of 31 -> 3.2% elapsed

	b := ComputeBudget(monthly(10, reset.Format(time.RFC3339Nano)), now)

	if !b.EarlyPeriod {
		t.Fatalf("EarlyPeriod = false, want true (elapsed %d%%)", b.ElapsedPct)
	}
	if b.DryDays <= dryDaysRed {
		t.Fatalf("DryDays = %.2f, want a red-worthy overshoot for this test to mean anything", b.DryDays)
	}
	if b.Level != LevelAmber {
		t.Errorf("Level = %q, want amber (pace-driven red capped early in the period)", b.Level)
	}

	// The absolute floor still bites through the cap.
	if got := ComputeBudget(monthly(96, reset.Format(time.RFC3339Nano)), now).Level; got != LevelRed {
		t.Errorf("Level at 96%% consumed = %q, want red despite the early-period cap", got)
	}
}

// Late in a period the pace maths mechanically compresses towards zero dry
// days, so consumption alone has to carry the warning.
func TestAbsoluteFloorLateInPeriod(t *testing.T) {
	reset := "2026-09-22T00:00:00Z"
	now := mustTime("2026-09-21T00:00:00Z") // 30 of 31 days elapsed

	b := ComputeBudget(monthly(97, reset), now)
	if b.DryDays > dryDaysRed {
		t.Fatalf("DryDays = %.3f, want at most %.0f so the red can only come from the absolute floor", b.DryDays, dryDaysRed)
	}
	if b.Level != LevelRed {
		t.Errorf("Level = %q, want red at 97%% consumed", b.Level)
	}

	if got := ComputeBudget(monthly(85, reset), now).Level; got != LevelAmber {
		t.Errorf("Level at 85%% consumed = %q, want amber", got)
	}
	if got := ComputeBudget(monthly(60, reset), now).Level; got != LevelGreen {
		t.Errorf("Level at 60%% consumed with 1 day left = %q, want green", got)
	}
}

func TestNoConsumptionIsGreen(t *testing.T) {
	b := ComputeBudget(monthly(0, "2026-09-22T00:00:00Z"), mustTime("2026-09-10T00:00:00Z"))

	if b.Level != LevelGreen {
		t.Errorf("Level = %q, want green", b.Level)
	}
	if b.DryDays != 0 {
		t.Errorf("DryDays = %.3f, want 0", b.DryDays)
	}
	if !b.WallAt.IsZero() {
		t.Errorf("WallAt = %s, want zero (no rate, no wall)", b.WallAt)
	}
	if b.ProjectedPct != 0 {
		t.Errorf("ProjectedPct = %d, want 0", b.ProjectedPct)
	}
}

func TestAtCeilingIsDryUntilReset(t *testing.T) {
	now := mustTime("2026-09-12T00:00:00Z")
	b := ComputeBudget(monthly(100, "2026-09-22T00:00:00Z"), now)

	if !b.AtCeiling {
		t.Error("AtCeiling = false, want true")
	}
	if b.Level != LevelRed {
		t.Errorf("Level = %q, want red", b.Level)
	}
	closeTo(t, "DryDays", b.DryDays, 10, 0.001)
	closeTo(t, "AllowedRatePerDay", b.AllowedRatePerDay, 0, 0.001)
}

// A stale cache or clock skew can put now past the reset. Elapsed is clamped to
// the period so the rate never goes negative or blows past the window.
func TestClampsPastReset(t *testing.T) {
	b := ComputeBudget(monthly(50, "2026-09-22T00:00:00Z"), mustTime("2026-09-25T00:00:00Z"))

	if !b.Valid {
		t.Fatal("Valid = false, want true")
	}
	if b.ElapsedPct != 100 {
		t.Errorf("ElapsedPct = %d, want 100", b.ElapsedPct)
	}
	if b.DaysLeft != 0 {
		t.Errorf("DaysLeft = %.3f, want 0", b.DaysLeft)
	}
	if b.DryDays != 0 {
		t.Errorf("DryDays = %.3f, want 0", b.DryDays)
	}
	if b.RatePerDay <= 0 {
		t.Errorf("RatePerDay = %.3f, want positive", b.RatePerDay)
	}
}

// Clock skew the other way: now sits before the derived period start. Elapsed
// is floored at zero so the rate can never go negative.
func TestClampsBeforePeriodStart(t *testing.T) {
	b := ComputeBudget(monthly(10, "2026-09-22T00:00:00Z"), mustTime("2026-08-01T00:00:00Z"))

	if !b.Valid {
		t.Fatal("Valid = false, want true")
	}
	if b.ElapsedDays != 0 {
		t.Errorf("ElapsedDays = %.3f, want 0", b.ElapsedDays)
	}
	if b.ElapsedPct != 0 {
		t.Errorf("ElapsedPct = %d, want 0", b.ElapsedPct)
	}
	if b.RatePerDay != 0 {
		t.Errorf("RatePerDay = %.3f, want 0 (no elapsed time, no measurable rate)", b.RatePerDay)
	}
	if b.DryDays != 0 {
		t.Errorf("DryDays = %.3f, want 0", b.DryDays)
	}
	if b.Level == LevelRed {
		t.Error("Level = red, want no pace-driven alarm with zero elapsed time")
	}
}

func TestZeroResetIsInvalid(t *testing.T) {
	if ComputeBudget(Window{Kind: KindMonthly, Percent: 50}, captureNow).Valid {
		t.Error("Valid = true, want false when resetsAt is missing")
	}
}

func TestAllowedRateIsWhatIsLeftOverTimeLeft(t *testing.T) {
	b := ComputeBudget(monthly(28, "2026-09-22T01:23:30.224Z"), captureNow)

	closeTo(t, "RatePerDay", b.RatePerDay, 7.194, 0.01)
	closeTo(t, "AllowedRatePerDay", b.AllowedRatePerDay, 2.656, 0.01)
	if b.AllowedRatePerDay >= b.RatePerDay {
		t.Error("allowed rate should be below the current rate when overshooting")
	}
}

func TestAbsoluteLevel(t *testing.T) {
	cases := []struct {
		pct  int
		want Level
	}{{0, LevelGreen}, {69, LevelGreen}, {70, LevelAmber}, {89, LevelAmber}, {90, LevelRed}, {100, LevelRed}}
	for _, tc := range cases {
		if got := AbsoluteLevel(tc.pct); got != tc.want {
			t.Errorf("AbsoluteLevel(%d) = %q, want %q", tc.pct, got, tc.want)
		}
	}
}

func TestSeverity(t *testing.T) {
	if LevelGreen.Severity() != 0 || LevelAmber.Severity() != 1 || LevelRed.Severity() != 2 {
		t.Error("severity mapping changed")
	}
}

func TestWorstLevel(t *testing.T) {
	cases := []struct {
		in   []Level
		want Level
	}{
		{[]Level{LevelGreen, LevelGreen}, LevelGreen},
		{[]Level{LevelGreen, LevelAmber}, LevelAmber},
		{[]Level{LevelAmber, LevelRed, LevelGreen}, LevelRed},
		{nil, LevelGreen},
	}
	for _, tc := range cases {
		if got := WorstLevel(tc.in...); got != tc.want {
			t.Errorf("WorstLevel(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}
