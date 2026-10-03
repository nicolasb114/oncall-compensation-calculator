// Package report fetches data from PagerDuty, classifies it and prices it.
package report

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"oncallcalc/internal/engine"
	"oncallcalc/internal/pd"
	"oncallcalc/internal/pricing"
)

// Person is one responder included in the report.
type Person struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	Team       string `json:"team,omitempty"`
	TimeZone   string `json:"timeZone"`             // from the PagerDuty user profile
	TZOverride string `json:"tzOverride,omitempty"` // optional override
	Country    string `json:"country,omitempty"`    // for holidays; guessed from time zone when empty
}

func (p Person) tz() string {
	if p.TZOverride != "" {
		return p.TZOverride
	}
	return p.TimeZone
}

// Config is one report run (everything the user chose).
type Config struct {
	Name           string         `json:"name"`
	From           string         `json:"from"` // YYYY-MM-DD, first day (inclusive)
	To             string         `json:"to"`   // YYYY-MM-DD, last day (inclusive)
	ReportTZ       string         `json:"reportTZ"`
	People         []Person       `json:"people"`
	ScheduleIDs    []string       `json:"scheduleIds,omitempty"`
	PolicyIDs      []string       `json:"policyIds,omitempty"`
	Rules          engine.Rules   `json:"rules"`
	HolidayCountry string         `json:"holidayCalendar"` // "country" (per person) or "manual"
	ManualHolidays []string       `json:"manualHolidays,omitempty"`
	IncludeEngaged bool           `json:"includeEngaged"`
	Pricing        pricing.Config `json:"pricing"`
}

// DayRow is one person's classified hours for one local day.
type DayRow struct {
	Date    string             `json:"date"`
	DayType string             `json:"dayType"`
	Hours   map[string]float64 `json:"hours"` // level 1, seconds
	L2      float64            `json:"l2"`    // level 2+, seconds
	Engaged float64            `json:"engaged"`
	Note    string             `json:"note,omitempty"`
}

// PersonResult is one row of the final report.
type PersonResult struct {
	Person
	L1         map[string]float64 `json:"l1"` // seconds per bucket, level 1
	L2         map[string]float64 `json:"l2"` // seconds per bucket, level 2+
	Engaged    map[string]float64 `json:"engaged"`
	Excluded   float64            `json:"excluded"`
	Amounts    *pricing.Amounts   `json:"amounts,omitempty"`
	Days       []DayRow           `json:"days"`
	Incomplete []string           `json:"incomplete,omitempty"`
}

// Result is the whole report.
type Result struct {
	Config   Config         `json:"config"`
	Created  time.Time      `json:"created"`
	People   []PersonResult `json:"people"`
	Warnings []string       `json:"warnings"`
	Stats    Stats          `json:"stats"`
}

// Stats summarizes the API activity of a run.
type Stats struct {
	Calls, Retries, RateLimitWaits int
}

// Progress is reported while running.
type Progress func(stage string, done, total int)

// Run executes a report.
func Run(ctx context.Context, cl *pd.Client, hol *Holidays, cfg Config, progress Progress) (*Result, error) {
	if err := cfg.Rules.Normalize(); err != nil {
		return nil, err
	}
	if len(cfg.People) == 0 {
		return nil, fmt.Errorf("no people selected")
	}
	repLoc, err := time.LoadLocation(cfg.ReportTZ)
	if err != nil {
		return nil, fmt.Errorf("invalid reporting time zone %q", cfg.ReportTZ)
	}
	fromD, err1 := time.ParseInLocation("2006-01-02", cfg.From, repLoc)
	toD, err2 := time.ParseInLocation("2006-01-02", cfg.To, repLoc)
	if err1 != nil || err2 != nil || toD.Before(fromD) {
		return nil, fmt.Errorf("invalid period %s to %s", cfg.From, cfg.To)
	}
	from, to := fromD, time.Date(toD.Year(), toD.Month(), toD.Day()+1, 0, 0, 0, 0, repLoc) // [from, to)
	res := &Result{Config: cfg, Created: time.Now()}

	ids := make([]string, len(cfg.People))
	for i, p := range cfg.People {
		ids[i] = p.ID
	}
	progress("Fetching on-call periods", 0, 1)
	oncalls, fails := cl.OnCalls(ctx, from, to, pd.OnCallFilters{UserIDs: ids, ScheduleIDs: cfg.ScheduleIDs, PolicyIDs: cfg.PolicyIDs})
	if ctx.Err() != nil {
		return nil, ctx.Err()
	}
	progress("Fetching on-call periods", 1, 1)
	failedUsers := map[string][]string{}
	for _, f := range fails {
		for _, u := range f.UserIDs {
			failedUsers[u] = append(failedUsers[u], fmt.Sprintf("on-call data %s to %s: %s", f.From.Format("01-02"), f.To.Format("01-02"), f.Err))
		}
	}

	byUser := map[string][]pd.OnCall{}
	for _, o := range oncalls {
		byUser[o.UserID] = append(byUser[o.UserID], o)
	}

	type uState struct {
		r      *PersonResult
		cls    *engine.Classifier
		l1, l2 []engine.Interval
	}
	states := make([]*uState, len(cfg.People))
	for i, p := range cfg.People {
		set := engine.HolidaySet{}
		if cfg.Rules.HolidayMode != "normal" {
			country := p.Country
			if country == "" {
				country = CountryForTZ(p.tz())
			}
			if cfg.HolidayCountry == "manual" {
				country = ""
			}
			var warns []string
			set, warns = hol.Set(ctx, country, from, to, cfg.ManualHolidays)
			for _, w := range warns {
				res.Warnings = appendUnique(res.Warnings, p.Name+": "+w)
			}
		}
		cls, err := engine.NewClassifier(cfg.Rules, p.tz(), set)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", p.Name, err)
		}
		st := &uState{cls: cls, r: &PersonResult{Person: p, L1: map[string]float64{}, L2: map[string]float64{}, Engaged: map[string]float64{}}}
		for _, o := range byUser[p.ID] {
			if o.Level <= 1 {
				st.l1 = append(st.l1, o.Interval)
			} else {
				st.l2 = append(st.l2, o.Interval)
			}
		}
		st.l1 = engine.Clip(engine.Merge(st.l1), from, to)
		st.l2 = engine.Clip(engine.Merge(st.l2), from, to)
		if cfg.Rules.Overlap == "highest" {
			st.l2 = engine.Subtract(st.l2, st.l1)
		}
		st.r.Incomplete = failedUsers[p.ID]
		states[i] = st
	}

	// Classify on-call time.
	for _, st := range states {
		h1, ex1 := st.cls.Classify(st.l1)
		h2, ex2 := st.cls.Classify(st.l2)
		st.r.L1, st.r.L2, st.r.Excluded = h1, h2, ex1+ex2
		st.r.Days = buildDays(st.cls, st.l1, st.l2, from, to, cfg.Rules)
	}

	// Engaged time: one Analytics lookup per distinct classified window (shared between people),
	// only for windows where someone was on call. Engaged time is attributed to the window in
	// which the incident falls.
	if cfg.IncludeEngaged {
		type win struct {
			from, to time.Time
			bucket   string
			users    map[string]*uState
		}
		wins := map[string]*win{}
		var order []string
		for _, st := range states {
			all := engine.Merge(append(append([]engine.Interval{}, st.l1...), st.l2...))
			for _, seg := range st.cls.DaySegments(from, to) {
				if seg.Excluded {
					continue
				}
				if !overlaps(all, seg.Interval) {
					continue
				}
				k := seg.Start.UTC().Format(time.RFC3339) + "|" + seg.End.UTC().Format(time.RFC3339) + "|" + seg.Bucket
				w := wins[k]
				if w == nil {
					w = &win{from: seg.Start, to: seg.End, bucket: seg.Bucket, users: map[string]*uState{}}
					wins[k] = w
					order = append(order, k)
				}
				w.users[st.r.ID] = st
			}
		}
		sort.Strings(order)
		for i, k := range order {
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			progress("Fetching engaged time", i, len(order))
			w := wins[k]
			uids := make([]string, 0, len(w.users))
			for id := range w.users {
				uids = append(uids, id)
			}
			sort.Strings(uids)
			secs, err := cl.EngagedSeconds(ctx, w.from, w.to, uids)
			if err != nil {
				if ctx.Err() != nil {
					return nil, ctx.Err()
				}
				for _, st := range w.users {
					st.r.Incomplete = append(st.r.Incomplete, fmt.Sprintf("engaged time %s: %v", w.from.In(repLoc).Format("01-02 15:04"), err))
				}
				continue
			}
			for id, s := range secs {
				if st, ok := w.users[id]; ok {
					st.r.Engaged[w.bucket] += s
					addDayEngaged(st.r, w.from, repLoc, st.cls.Location(), s)
				}
			}
		}
		progress("Fetching engaged time", len(order), len(order))
	}

	// Pricing.
	for _, st := range states {
		if cfg.Pricing.Enabled {
			a := cfg.Pricing.Price(st.r.L1, st.r.L2, st.r.Engaged)
			st.r.Amounts = &a
		}
		res.People = append(res.People, *st.r)
	}
	res.Stats = Stats{cl.Calls, cl.Retries, cl.Waits}
	return res, nil
}

func overlaps(set []engine.Interval, iv engine.Interval) bool {
	for _, s := range set {
		if s.Start.Before(iv.End) && s.End.After(iv.Start) {
			return true
		}
	}
	return false
}

func appendUnique(s []string, v string) []string {
	for _, x := range s {
		if x == v {
			return s
		}
	}
	return append(s, v)
}

func addDayEngaged(r *PersonResult, at time.Time, _ *time.Location, loc *time.Location, secs float64) {
	d := at.In(loc).Format("2006-01-02")
	for i := range r.Days {
		if r.Days[i].Date == d {
			r.Days[i].Engaged += secs
			return
		}
	}
	r.Days = append(r.Days, DayRow{Date: d, Hours: map[string]float64{}, DayType: at.In(loc).Weekday().String()[:3], Engaged: secs})
	sort.Slice(r.Days, func(i, j int) bool { return r.Days[i].Date < r.Days[j].Date })
}

// buildDays gives the day-by-day view in the responder's local days.
func buildDays(c *engine.Classifier, l1, l2 []engine.Interval, from, to time.Time, rules engine.Rules) []DayRow {
	loc := c.Location()
	days := map[string]*DayRow{}
	var order []string
	get := func(t time.Time) *DayRow {
		d := t.In(loc).Format("2006-01-02")
		r := days[d]
		if r == nil {
			r = &DayRow{Date: d, Hours: map[string]float64{}, DayType: t.In(loc).Weekday().String()[:3]}
			days[d] = r
			order = append(order, d)
		}
		return r
	}
	for _, iv := range l1 {
		for _, p := range c.Split(iv) {
			// pieces never cross local midnight, so each belongs to one local day
			r := get(p.Start)
			if p.Excluded {
				r.Note = "excluded holiday"
				continue
			}
			r.Hours[p.Bucket] += p.Seconds()
			if p.Bucket == engine.Holiday || p.Bucket == engine.HolidayNight {
				r.Note = "holiday"
			}
		}
	}
	for _, iv := range l2 {
		for _, p := range c.Split(iv) {
			if !p.Excluded {
				get(p.Start).L2 += p.Seconds()
			}
		}
	}
	sort.Strings(order)
	out := make([]DayRow, 0, len(order))
	for _, d := range order {
		out = append(out, *days[d])
	}
	return out
}

// Tidy trims names for display.
func Tidy(s string) string { return strings.TrimSpace(s) }
