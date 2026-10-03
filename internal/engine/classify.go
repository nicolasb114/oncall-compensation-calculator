package engine

import (
	"sort"
	"time"
)

// Interval is a half-open time span [Start, End).
type Interval struct {
	Start time.Time `json:"start"`
	End   time.Time `json:"end"`
}

// Seconds returns the interval length.
func (i Interval) Seconds() float64 { return i.End.Sub(i.Start).Seconds() }

// Piece is a span with one classification.
type Piece struct {
	Interval
	Bucket   string // "" and Excluded=true when dropped by the holiday "exclude" mode
	Excluded bool
}

// HolidaySet answers whether a local date (YYYY-MM-DD) is a holiday.
type HolidaySet map[string]bool

// Classifier classifies intervals for one responder.
type Classifier struct {
	r        Rules
	loc      *time.Location
	holidays HolidaySet
	bs, be   int
	ns, ne   int
	days     map[int]bool
	weekend  map[int]bool
	perDay   map[int][2]int
	brk      map[int][2]int
	defBrk   [2]int
}

// NewClassifier builds a classifier for a responder time zone and holiday set.
func NewClassifier(r Rules, responderTZ string, holidays HolidaySet) (*Classifier, error) {
	if err := r.Normalize(); err != nil {
		return nil, err
	}
	tz := responderTZ
	if r.TZMode == "fixed" || tz == "" {
		if r.TZMode == "fixed" {
			tz = r.FixedTZ
		} else {
			tz = "UTC"
		}
	}
	loc, err := time.LoadLocation(tz)
	if err != nil {
		return nil, err
	}
	c := &Classifier{r: r, loc: loc, holidays: holidays,
		days: map[int]bool{}, weekend: map[int]bool{}, perDay: map[int][2]int{}, brk: map[int][2]int{}}
	c.bs, _ = parseClock(r.BusinessStart)
	c.be, _ = parseClock(r.BusinessEnd)
	c.ns, _ = parseClock(r.NightStart)
	c.ne, _ = parseClock(r.NightEnd)
	for _, d := range r.BusinessDays {
		c.days[d] = true
	}
	for _, d := range r.WeekendDays {
		c.weekend[d] = true
	}
	if r.BreakStart != "" && r.BreakEnd != "" {
		a, _ := parseClock(r.BreakStart)
		b, _ := parseClock(r.BreakEnd)
		c.defBrk = [2]int{a, b}
	}
	for k, w := range r.PerDay {
		var wd int
		if _, e := fmtSscan(k, &wd); e != nil {
			continue
		}
		s, e1 := parseClock(w.Start)
		en, e2 := parseClock(w.End)
		if e1 == nil && e2 == nil {
			c.perDay[wd] = [2]int{s, en}
		}
		if w.BreakStart != "" && w.BreakEnd != "" {
			a, e3 := parseClock(w.BreakStart)
			b, e4 := parseClock(w.BreakEnd)
			if e3 == nil && e4 == nil {
				c.brk[wd] = [2]int{a, b}
			}
		}
	}
	return c, nil
}

// Location returns the time zone used for classification.
func (c *Classifier) Location() *time.Location { return c.loc }

func (c *Classifier) window(wd int) (int, int) {
	if w, ok := c.perDay[wd]; ok {
		return w[0], w[1]
	}
	return c.bs, c.be
}

func (c *Classifier) breakFor(wd int) (int, int, bool) {
	if b, ok := c.brk[wd]; ok {
		return b[0], b[1], true
	}
	if c.defBrk != [2]int{} {
		return c.defBrk[0], c.defBrk[1], true
	}
	return 0, 0, false
}

func inWindow(m, s, e int) bool {
	if s == e {
		return false
	}
	if s < e {
		return m >= s && m < e
	}
	return m >= s || m < e // wraps midnight
}

// classifyAt returns the bucket for the local instant t (attributes are constant within a piece).
func (c *Classifier) classifyAt(t time.Time) (bucket string, excluded bool) {
	l := t.In(c.loc)
	m := l.Hour()*60 + l.Minute()
	wd := int(l.Weekday())
	hol := c.r.HolidayMode != "normal" && c.holidays[l.Format("2006-01-02")]
	night := inWindow(m, c.ns, c.ne)
	isWeekend := c.weekend[wd]
	bs, be := c.window(wd)
	business := c.days[wd] && !isWeekend && inWindow(m, bs, be)
	if business {
		if b1, b2, ok := c.breakFor(wd); ok && inWindow(m, b1, b2) {
			business = false
		}
	}
	for _, p := range c.r.Precedence {
		switch p {
		case "holiday":
			if hol {
				if c.r.HolidayMode == "exclude" {
					return "", true
				}
				if night && c.r.SplitHolidayNight {
					return HolidayNight, false
				}
				return Holiday, false
			}
		case "weekend":
			if isWeekend {
				if night {
					return WeekendNight, false
				}
				return Weekend, false
			}
		case "night":
			if night {
				return Night, false
			}
		case "nonbusiness":
			if !business {
				return NonBusiness, false
			}
		case "business":
			if business {
				return Business, false
			}
		}
	}
	return NonBusiness, false
}

// cutPoints returns the local-clock boundaries that can change classification within a day.
func (c *Classifier) cutMinutes(wd int) []int {
	bs, be := c.window(wd)
	cuts := []int{0, bs, be, c.ns, c.ne}
	if b1, b2, ok := c.breakFor(wd); ok {
		cuts = append(cuts, b1, b2)
	}
	return cuts
}

// Split cuts an interval into classified pieces. Pieces are contiguous and sum to the interval.
func (c *Classifier) Split(iv Interval) []Piece {
	var out []Piece
	cur := iv.Start
	for cur.Before(iv.End) {
		l := cur.In(c.loc)
		y, mo, d := l.Date()
		dayEnd := time.Date(y, mo, d+1, 0, 0, 0, 0, c.loc)
		next := dayEnd
		for _, m := range c.cutMinutes(int(l.Weekday())) {
			if m >= 24*60 || m <= 0 {
				continue
			}
			t := time.Date(y, mo, d, m/60, m%60, 0, 0, c.loc)
			if t.After(cur) && t.Before(next) {
				next = t
			}
		}
		if next.After(iv.End) {
			next = iv.End
		}
		b, ex := c.classifyAt(cur)
		out = append(out, Piece{Interval: Interval{cur, next}, Bucket: b, Excluded: ex})
		cur = next
	}
	return out
}

// DaySegments returns merged classified segments covering the whole span [from, to),
// used to define the windows for per-bucket engaged-time lookups.
func (c *Classifier) DaySegments(from, to time.Time) []Piece {
	var merged []Piece
	for _, p := range c.Split(Interval{from, to}) {
		if n := len(merged); n > 0 && merged[n-1].Bucket == p.Bucket && merged[n-1].Excluded == p.Excluded && merged[n-1].End.Equal(p.Start) {
			merged[n-1].End = p.End
			continue
		}
		merged = append(merged, p)
	}
	return merged
}

// Merge unions overlapping intervals so overlapping schedules are never double counted.
func Merge(in []Interval) []Interval {
	if len(in) == 0 {
		return nil
	}
	s := append([]Interval(nil), in...)
	sort.Slice(s, func(i, j int) bool { return s[i].Start.Before(s[j].Start) })
	out := []Interval{s[0]}
	for _, iv := range s[1:] {
		last := &out[len(out)-1]
		if !iv.Start.After(last.End) {
			if iv.End.After(last.End) {
				last.End = iv.End
			}
			continue
		}
		out = append(out, iv)
	}
	return out
}

// Subtract removes the union of cut from base.
func Subtract(base, cut []Interval) []Interval {
	cut = Merge(cut)
	var out []Interval
	for _, b := range base {
		cur := b.Start
		for _, c := range cut {
			if !c.End.After(cur) || !c.Start.Before(b.End) {
				continue
			}
			if c.Start.After(cur) {
				out = append(out, Interval{cur, c.Start})
			}
			if c.End.After(cur) {
				cur = c.End
			}
		}
		if cur.Before(b.End) {
			out = append(out, Interval{cur, b.End})
		}
	}
	return out
}

// Clip restricts intervals to [from, to).
func Clip(in []Interval, from, to time.Time) []Interval {
	var out []Interval
	for _, iv := range in {
		s, e := iv.Start, iv.End
		if s.Before(from) {
			s = from
		}
		if e.After(to) {
			e = to
		}
		if s.Before(e) {
			out = append(out, Interval{s, e})
		}
	}
	return out
}

// Hours is seconds per bucket.
type Hours map[string]float64

// Total sums all buckets.
func (h Hours) Total() float64 {
	t := 0.0
	for _, v := range h {
		t += v
	}
	return t
}

// Classify sums classified seconds per bucket (and excluded seconds) for the merged intervals.
func (c *Classifier) Classify(in []Interval) (h Hours, excludedSecs float64) {
	h = Hours{}
	for _, iv := range Merge(in) {
		for _, p := range c.Split(iv) {
			if p.Excluded {
				excludedSecs += p.Seconds()
				continue
			}
			h[p.Bucket] += p.Seconds()
		}
	}
	return h, excludedSecs
}
