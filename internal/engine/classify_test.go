package engine

import (
	"math"
	"testing"
	"time"
)

func mustLoc(t *testing.T, n string) *time.Location {
	l, err := time.LoadLocation(n)
	if err != nil {
		t.Fatal(err)
	}
	return l
}

func newC(t *testing.T, tz string, mod func(*Rules), hol HolidaySet) *Classifier {
	r := DefaultRules()
	if mod != nil {
		mod(&r)
	}
	c, err := NewClassifier(r, tz, hol)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func hrs(h Hours, b string) float64 { return h[b] / 3600 }

func near(a, b float64) bool { return math.Abs(a-b) < 1e-6 }

// A full normal week (Mon-Sun) in UTC.
func TestWeekBuckets(t *testing.T) {
	c := newC(t, "UTC", nil, nil)
	from := time.Date(2026, 9, 14, 0, 0, 0, 0, time.UTC) // Monday
	h, ex := c.Classify([]Interval{{from, from.AddDate(0, 0, 7)}})
	if ex != 0 {
		t.Fatalf("excluded %v", ex)
	}
	// 5 weekdays: business 8h, night 8h (22-06), non-business 8h (06-09=3, 17-22=5)
	if !near(hrs(h, Business), 40) || !near(hrs(h, Night), 40) || !near(hrs(h, NonBusiness), 40) {
		t.Fatalf("weekday buckets wrong: %v", h)
	}
	// Weekend: 2 days; night 8h per day, rest 16h.
	if !near(hrs(h, Weekend), 32) || !near(hrs(h, WeekendNight), 16) {
		t.Fatalf("weekend buckets wrong: %v", h)
	}
	if !near(h.Total()/3600, 168) {
		t.Fatalf("total %v != 168", h.Total()/3600)
	}
}

// Buckets must always sum to the interval length, including across DST changes.
func TestSumInvariantDST(t *testing.T) {
	for _, tz := range []string{"America/New_York", "Europe/Paris", "Asia/Kolkata", "Australia/Lord_Howe", "America/Lima", "Pacific/Chatham"} {
		c := newC(t, tz, nil, HolidaySet{"2026-03-29": true, "2026-11-01": true})
		start := time.Date(2026, 3, 1, 0, 0, 0, 0, time.UTC)
		for _, span := range []int{20, 60, 250} {
			iv := Interval{start, start.AddDate(0, 0, span)}
			h, ex := c.Classify([]Interval{iv})
			if !near((h.Total()+ex)/3600, iv.Seconds()/3600) {
				t.Fatalf("%s span %d: %v+%v != %v", tz, span, h.Total()/3600, ex/3600, iv.Seconds()/3600)
			}
		}
	}
}

func TestDSTShortDay(t *testing.T) {
	// 2026-03-08 is a 23-hour day in New York.
	c := newC(t, "America/New_York", nil, nil)
	loc := mustLoc(t, "America/New_York")
	from := time.Date(2026, 3, 8, 0, 0, 0, 0, loc) // Sunday
	h, _ := c.Classify([]Interval{{from, time.Date(2026, 3, 9, 0, 0, 0, 0, loc)}})
	if !near(h.Total()/3600, 23) {
		t.Fatalf("expected 23h, got %v", h.Total()/3600)
	}
}

func TestResponderTimeZone(t *testing.T) {
	// 14:00 UTC Monday: business in London-ish (UTC), night in Tokyo (23:00).
	iv := Interval{time.Date(2026, 9, 14, 14, 0, 0, 0, time.UTC), time.Date(2026, 9, 14, 15, 0, 0, 0, time.UTC)}
	h, _ := newC(t, "UTC", nil, nil).Classify([]Interval{iv})
	if !near(hrs(h, Business), 1) {
		t.Fatalf("UTC: %v", h)
	}
	h, _ = newC(t, "Asia/Tokyo", nil, nil).Classify([]Interval{iv})
	if !near(hrs(h, Night), 1) {
		t.Fatalf("Tokyo: %v", h)
	}
	// Fixed time zone overrides the responder's zone.
	h, _ = newC(t, "Asia/Tokyo", func(r *Rules) { r.TZMode = "fixed"; r.FixedTZ = "UTC" }, nil).Classify([]Interval{iv})
	if !near(hrs(h, Business), 1) {
		t.Fatalf("fixed: %v", h)
	}
}

func TestHolidayModes(t *testing.T) {
	hol := HolidaySet{"2026-09-16": true} // Wednesday
	day := Interval{time.Date(2026, 9, 16, 0, 0, 0, 0, time.UTC), time.Date(2026, 9, 17, 0, 0, 0, 0, time.UTC)}
	h, _ := newC(t, "UTC", nil, hol).Classify([]Interval{day})
	if !near(hrs(h, Holiday), 24) {
		t.Fatalf("bucket mode: %v", h)
	}
	h, _ = newC(t, "UTC", func(r *Rules) { r.SplitHolidayNight = true }, hol).Classify([]Interval{day})
	if !near(hrs(h, Holiday), 16) || !near(hrs(h, HolidayNight), 8) {
		t.Fatalf("split: %v", h)
	}
	h, _ = newC(t, "UTC", func(r *Rules) { r.HolidayMode = "normal" }, hol).Classify([]Interval{day})
	if !near(hrs(h, Business), 8) || hrs(h, Holiday) != 0 {
		t.Fatalf("normal: %v", h)
	}
	h, ex := newC(t, "UTC", func(r *Rules) { r.HolidayMode = "exclude" }, hol).Classify([]Interval{day})
	if h.Total() != 0 || !near(ex/3600, 24) {
		t.Fatalf("exclude: %v %v", h, ex)
	}
}

func TestPrecedenceAndBreak(t *testing.T) {
	// Saturday: weekend wins over night/non-business by default.
	sat := Interval{time.Date(2026, 9, 19, 23, 0, 0, 0, time.UTC), time.Date(2026, 9, 19, 23, 30, 0, 0, time.UTC)}
	h, _ := newC(t, "UTC", nil, nil).Classify([]Interval{sat})
	if !near(hrs(h, WeekendNight), 0.5) {
		t.Fatalf("%v", h)
	}
	// Night before weekend when precedence is changed.
	h, _ = newC(t, "UTC", func(r *Rules) { r.Precedence = []string{"holiday", "night", "weekend", "nonbusiness", "business"} }, nil).Classify([]Interval{sat})
	if !near(hrs(h, Night), 0.5) {
		t.Fatalf("%v", h)
	}
	// Lunch break turns business hour into non-business.
	mon := Interval{time.Date(2026, 9, 14, 12, 0, 0, 0, time.UTC), time.Date(2026, 9, 14, 14, 0, 0, 0, time.UTC)}
	h, _ = newC(t, "UTC", func(r *Rules) { r.BreakStart = "12:00"; r.BreakEnd = "13:00" }, nil).Classify([]Interval{mon})
	if !near(hrs(h, NonBusiness), 1) || !near(hrs(h, Business), 1) {
		t.Fatalf("%v", h)
	}
}

func TestMergeSubtractNoDoubleCount(t *testing.T) {
	d := func(h int) time.Time { return time.Date(2026, 9, 14, h, 0, 0, 0, time.UTC) }
	m := Merge([]Interval{{d(1), d(5)}, {d(4), d(8)}, {d(10), d(12)}})
	if len(m) != 2 || !m[0].End.Equal(d(8)) {
		t.Fatalf("%v", m)
	}
	s := Subtract([]Interval{{d(0), d(10)}}, []Interval{{d(2), d(4)}, {d(6), d(7)}})
	total := 0.0
	for _, i := range s {
		total += i.Seconds() / 3600
	}
	if !near(total, 7) {
		t.Fatalf("%v", s)
	}
}

func TestPerDayWindow(t *testing.T) {
	// Short Friday: business ends 13:00.
	c := newC(t, "UTC", func(r *Rules) { r.PerDay = map[string]DayWindow{"5": {Start: "09:00", End: "13:00"}} }, nil)
	fri := Interval{time.Date(2026, 9, 18, 9, 0, 0, 0, time.UTC), time.Date(2026, 9, 18, 17, 0, 0, 0, time.UTC)}
	h, _ := c.Classify([]Interval{fri})
	if !near(hrs(h, Business), 4) || !near(hrs(h, NonBusiness), 4) {
		t.Fatalf("%v", h)
	}
}
