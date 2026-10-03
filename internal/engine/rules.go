// Package engine classifies on-call time into hour types using adjustable rules,
// evaluated in each responder's own time zone.
package engine

import (
	"fmt"
	"strings"
	"time"
)

// Hour-type buckets. Every on-call second lands in exactly one of them.
const (
	Business     = "business"
	NonBusiness  = "non_business"
	Night        = "night"
	Weekend      = "weekend"
	WeekendNight = "weekend_night"
	Holiday      = "holiday"
	HolidayNight = "holiday_night"
)

// Buckets lists all buckets in display order.
var Buckets = []string{Business, NonBusiness, Night, Weekend, WeekendNight, Holiday, HolidayNight}

// DayWindow is a business window for one weekday, with an optional break.
type DayWindow struct {
	Start      string `json:"start"` // "09:00"
	End        string `json:"end"`   // "17:00"
	BreakStart string `json:"breakStart,omitempty"`
	BreakEnd   string `json:"breakEnd,omitempty"`
}

// Rules is the full classification rule set (one "profile").
type Rules struct {
	BusinessStart string `json:"businessStart"`
	BusinessEnd   string `json:"businessEnd"`
	BreakStart    string `json:"breakStart,omitempty"`
	BreakEnd      string `json:"breakEnd,omitempty"`
	BusinessDays  []int  `json:"businessDays"` // 0=Sunday .. 6=Saturday
	// PerDay overrides the window for a weekday (key "0".."6").
	PerDay map[string]DayWindow `json:"perDay,omitempty"`

	NightStart string `json:"nightStart"`
	NightEnd   string `json:"nightEnd"`

	WeekendDays []int `json:"weekendDays"`

	// HolidayMode: "bucket" (own bucket), "normal" (treated as a normal day), "exclude".
	HolidayMode       string `json:"holidayMode"`
	SplitHolidayNight bool   `json:"splitHolidayNight"`
	// Precedence is the order in which day types win. Default: holiday, weekend, night, nonbusiness, business.
	Precedence []string `json:"precedence,omitempty"`

	// TZMode: "responder" (default) or "fixed" (use FixedTZ for everyone).
	TZMode  string `json:"tzMode"`
	FixedTZ string `json:"fixedTZ,omitempty"`

	// Overlap: how a responder on call at level 1 and level 2+ at once is counted: "both" or "highest".
	Overlap string `json:"overlap"`
}

// DefaultRules returns sensible defaults.
func DefaultRules() Rules {
	return Rules{
		BusinessStart: "09:00", BusinessEnd: "17:00",
		BusinessDays: []int{1, 2, 3, 4, 5},
		NightStart:   "22:00", NightEnd: "06:00",
		WeekendDays: []int{0, 6},
		HolidayMode: "bucket", TZMode: "responder", Overlap: "both",
		Precedence: []string{"holiday", "weekend", "night", "nonbusiness", "business"},
	}
}

// Normalize fills blanks with defaults and validates the rules.
func (r *Rules) Normalize() error {
	d := DefaultRules()
	if r.BusinessStart == "" {
		r.BusinessStart = d.BusinessStart
	}
	if r.BusinessEnd == "" {
		r.BusinessEnd = d.BusinessEnd
	}
	if r.NightStart == "" {
		r.NightStart = d.NightStart
	}
	if r.NightEnd == "" {
		r.NightEnd = d.NightEnd
	}
	if r.BusinessDays == nil {
		r.BusinessDays = d.BusinessDays
	}
	if r.WeekendDays == nil {
		r.WeekendDays = d.WeekendDays
	}
	if r.HolidayMode == "" {
		r.HolidayMode = d.HolidayMode
	}
	if r.TZMode == "" {
		r.TZMode = d.TZMode
	}
	if r.Overlap == "" {
		r.Overlap = d.Overlap
	}
	if len(r.Precedence) == 0 {
		r.Precedence = d.Precedence
	}
	for _, p := range r.Precedence {
		switch p {
		case "holiday", "weekend", "night", "nonbusiness", "business":
		default:
			return fmt.Errorf("unknown precedence entry %q", p)
		}
	}
	for _, v := range []string{r.BusinessStart, r.BusinessEnd, r.NightStart, r.NightEnd} {
		if _, err := parseClock(v); err != nil {
			return err
		}
	}
	if r.TZMode == "fixed" {
		if _, err := time.LoadLocation(r.FixedTZ); err != nil {
			return fmt.Errorf("invalid fixed time zone %q", r.FixedTZ)
		}
	}
	switch r.HolidayMode {
	case "bucket", "normal", "exclude":
	default:
		return fmt.Errorf("unknown holiday mode %q", r.HolidayMode)
	}
	return nil
}

// minutes since midnight
func parseClock(s string) (int, error) {
	s = strings.TrimSpace(s)
	var h, m int
	if _, err := fmt.Sscanf(s, "%d:%d", &h, &m); err != nil || h < 0 || h > 24 || m < 0 || m > 59 {
		return 0, fmt.Errorf("invalid time %q (use HH:MM)", s)
	}
	return h*60 + m, nil
}
