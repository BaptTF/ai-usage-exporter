// Package budget derives the pace of a quota window: how many days will be
// spent at the ceiling before the reset, and a traffic-light severity.
//
// This is the business logic preserved from the legacy dashboard. The period
// start is derived from the reset instant per window kind:
//
//	monthly — subscription anniversary (one calendar month back)
//	weekly  — seven days back (calendar-aligned Monday for OpenCode)
//	daily   — twenty-four hours back (Devin seat quota)
//	rolling — no derivable start, so no pace maths at all
package budget

import (
	"math"
	"time"
)

// Level is the traffic-light severity of a quota window.
type Level string

const (
	LevelGreen Level = "green"
	LevelAmber Level = "amber"
	LevelRed   Level = "red"
)

// Severity maps a level to the metric value documented by the exporter:
// 0 green, 1 amber, 2 red.
func (l Level) Severity() int {
	switch l {
	case LevelRed:
		return 2
	case LevelAmber:
		return 1
	default:
		return 0
	}
}

// Window kinds. Each kind has its own period-start rule.
const (
	KindMonthly = "monthly"
	KindWeekly  = "weekly"
	KindDaily   = "daily"
	KindRolling = "rolling"
)

// earlyPeriodFraction is the fraction of the period below which the average
// burn rate is dominated by a single session. The projection stays visible but
// must not raise a red light on its own before that point.
const earlyPeriodFraction = 0.05

// Traffic-light thresholds, expressed in dry days and absolute consumption.
const (
	dryDaysRed       = 3.0
	consumedAmberPct = 80
	consumedRedPct   = 95
)

// Window is the input to the budget computation.
type Window struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	Status   string    `json:"status"`
	Percent  int       `json:"percent"`
	ResetsAt time.Time `json:"resetsAt"`
}

// Budget answers "where are we versus the reset?" for one quota window.
// It is derived entirely from percent consumed and the reset instant.
type Budget struct {
	Valid bool `json:"valid"`

	PeriodStart time.Time `json:"periodStart"`
	PeriodDays  float64   `json:"periodDays"`
	ElapsedDays float64   `json:"elapsedDays"`
	DaysLeft    float64   `json:"daysLeft"`

	ElapsedPct  int `json:"elapsedPct"`
	ConsumedPct int `json:"consumedPct"`
	AheadPct    int `json:"aheadPct"`

	RatePerDay        float64 `json:"ratePerDay"`
	AllowedRatePerDay float64 `json:"allowedRatePerDay"`
	ProjectedPct      int     `json:"projectedPct"`

	WallAt    time.Time `json:"wallAt,omitempty"`
	DryDays   float64   `json:"dryDays"`
	AtCeiling bool      `json:"atCeiling"`

	EarlyPeriod bool  `json:"earlyPeriod"`
	Level       Level `json:"level"`
}

// ComputeBudget derives the budget position of a window at instant now.
func ComputeBudget(w Window, now time.Time) Budget {
	b := Budget{ConsumedPct: w.Percent, Level: LevelGreen}

	start, ok := periodStart(w)
	if !ok {
		return b // Valid stays false: no pace maths for this window.
	}

	total := w.ResetsAt.Sub(start)
	if total <= 0 {
		return b
	}

	elapsed := now.Sub(start)
	if elapsed < 0 {
		elapsed = 0
	}
	if elapsed > total {
		elapsed = total
	}

	b.Valid = true
	b.PeriodStart = start
	b.PeriodDays = total.Hours() / 24
	b.ElapsedDays = elapsed.Hours() / 24
	b.DaysLeft = math.Max(0, b.PeriodDays-b.ElapsedDays)

	frac := float64(elapsed) / float64(total)
	b.ElapsedPct = int(math.Round(frac * 100))
	b.EarlyPeriod = frac < earlyPeriodFraction
	b.AheadPct = b.ConsumedPct - b.ElapsedPct

	consumed := float64(w.Percent)
	remaining := math.Max(0, 100-consumed)
	b.AtCeiling = consumed >= 100

	if b.ElapsedDays > 0 {
		b.RatePerDay = consumed / b.ElapsedDays
		b.ProjectedPct = int(math.Round(b.RatePerDay * b.PeriodDays))
	}
	if b.DaysLeft > 0 {
		b.AllowedRatePerDay = remaining / b.DaysLeft
	}

	switch {
	case b.AtCeiling:
		// Already out of quota: dry until the reset.
		b.WallAt = now
		b.DryDays = b.DaysLeft
	case b.RatePerDay > 0:
		daysToWall := remaining / b.RatePerDay
		b.WallAt = now.Add(time.Duration(daysToWall * 24 * float64(time.Hour)))
		if daysToWall < b.DaysLeft {
			b.DryDays = b.DaysLeft - daysToWall
		}
	}

	b.Level = computeLevel(b)
	return b
}

func computeLevel(b Budget) Level {
	level := LevelGreen
	switch {
	case b.DryDays > dryDaysRed:
		level = LevelRed
	case b.DryDays > 0:
		level = LevelAmber
	}

	// Absolute floor, independent of pace.
	if b.ConsumedPct >= consumedRedPct {
		level = LevelRed
	} else if b.ConsumedPct >= consumedAmberPct && level == LevelGreen {
		level = LevelAmber
	}

	// At the very start of a period a single heavy session projects to a wild
	// overshoot. Cap the pace-driven red at amber; the absolute floor above can
	// still force red on its own.
	if b.EarlyPeriod && level == LevelRed && b.ConsumedPct < consumedRedPct {
		level = LevelAmber
	}

	return level
}

// AbsoluteLevel grades a window on raw consumption only. Used for the rolling
// 5h window, where hitting the ceiling blocks you right now and the notion of
// pace carries no meaning.
func AbsoluteLevel(pct int) Level {
	switch {
	case pct >= 90:
		return LevelRed
	case pct >= 70:
		return LevelAmber
	default:
		return LevelGreen
	}
}

// WorstLevel returns the most severe level of those given.
func WorstLevel(levels ...Level) Level {
	worst := LevelGreen
	for _, l := range levels {
		if l.Severity() > worst.Severity() {
			worst = l
		}
	}
	return worst
}

// periodStart derives the instant the window opened, per kind.
func periodStart(w Window) (time.Time, bool) {
	if w.ResetsAt.IsZero() {
		return time.Time{}, false
	}
	switch w.Kind {
	case KindMonthly:
		return monthlyStart(w.ResetsAt), true
	case KindWeekly:
		return w.ResetsAt.AddDate(0, 0, -7), true
	case KindDaily:
		return w.ResetsAt.Add(-24 * time.Hour), true
	default:
		// Rolling 5h: with zero usage the API returns now+5h, which would put
		// the period start at "now" and make the rate infinite.
		return time.Time{}, false
	}
}

// monthlyStart steps back one calendar month from the reset instant, keeping
// the subscription anniversary.
func monthlyStart(reset time.Time) time.Time {
	start := reset.AddDate(0, -1, 0)
	// AddDate normalises overflow: one month before 31 March is "31 February",
	// which Go rolls forward into March. Clamp back to the last day of the
	// shorter month so the period keeps a plausible length.
	if start.Day() != reset.Day() {
		start = start.AddDate(0, 0, -start.Day())
	}
	return start
}
