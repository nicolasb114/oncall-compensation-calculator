// Package pricing turns classified hours into compensation estimates.
package pricing

import (
	"math"

	"oncallcalc/internal/engine"
)

// RateSet is one set of rates (used for level 1, and optionally level 2+).
type RateSet struct {
	Base      float64            `json:"base"`
	Surcharge map[string]float64 `json:"surcharge"` // percent over base, per bucket (style "surcharge")
	Rate      map[string]float64 `json:"rate"`      // amount per hour, per bucket (style "rate")
	// Engaged: surcharge percent over base, or an explicit rate.
	EngagedSurcharge float64 `json:"engagedSurcharge"`
	EngagedRate      float64 `json:"engagedRate"`
}

// Config is the full pricing setup. When Enabled is false the report is hours only.
type Config struct {
	Enabled  bool   `json:"enabled"`
	Style    string `json:"style"` // "surcharge" or "rate"
	Currency string `json:"currency"`
	// Stack decides combined buckets (weekend night, holiday night) in surcharge style:
	// "add" (sum of the two surcharges), "max" (highest wins) or "custom" (use the bucket's own value).
	Stack string  `json:"stack"`
	L1    RateSet `json:"l1"`
	// Level2 "same" (default) uses L1's rates; "separate" uses L2.
	Level2 string  `json:"level2"`
	L2     RateSet `json:"l2"`

	StipendPerPerson float64 `json:"stipendPerPerson"` // flat amount per person per report
	CapPerPerson     float64 `json:"capPerPerson"`     // 0 = none
	FloorPerPerson   float64 `json:"floorPerPerson"`   // 0 = none
	RoundMode        string  `json:"roundMode"`        // "none", "nearest", "up"
	RoundMinutes     int     `json:"roundMinutes"`     // e.g. 15
}

// Default returns the "base + surcharge" example: 5/hour, +25% outside business, +50% night.
func Default() Config {
	return Config{
		Enabled: true, Style: "surcharge", Currency: "USD", Stack: "add", Level2: "same", RoundMode: "none",
		L1: RateSet{Base: 5,
			Surcharge: map[string]float64{
				engine.Business: 0, engine.NonBusiness: 25, engine.Night: 50,
				engine.Weekend: 25, engine.WeekendNight: 75, engine.Holiday: 100, engine.HolidayNight: 150},
			EngagedSurcharge: 100},
	}
}

func (c Config) set(level2 bool) RateSet {
	if level2 && c.Level2 == "separate" {
		return c.L2
	}
	return c.L1
}

// EffectiveRate is the amount per hour for a bucket.
func (c Config) EffectiveRate(level2 bool, bucket string) float64 {
	rs := c.set(level2)
	if c.Style == "rate" {
		return rs.Rate[bucket]
	}
	pct := rs.Surcharge[bucket]
	if c.Stack != "custom" {
		switch bucket {
		case engine.WeekendNight:
			pct = combine(c.Stack, rs.Surcharge[engine.Weekend], rs.Surcharge[engine.Night])
		case engine.HolidayNight:
			pct = combine(c.Stack, rs.Surcharge[engine.Holiday], rs.Surcharge[engine.Night])
		}
	}
	return rs.Base * (1 + pct/100)
}

func combine(mode string, a, b float64) float64 {
	if mode == "max" {
		return math.Max(a, b)
	}
	return a + b
}

// EngagedRate is the amount per engaged hour.
func (c Config) EngagedRate(level2 bool) float64 {
	rs := c.set(level2)
	if c.Style == "rate" {
		return rs.EngagedRate
	}
	return rs.Base * (1 + rs.EngagedSurcharge/100)
}

// RoundSeconds applies the rounding rule to a duration in seconds.
func (c Config) RoundSeconds(s float64) float64 {
	if c.RoundMode == "none" || c.RoundMode == "" || c.RoundMinutes <= 0 {
		return s
	}
	step := float64(c.RoundMinutes) * 60
	if c.RoundMode == "up" {
		return math.Ceil(s/step-1e-9) * step
	}
	return math.Round(s/step) * step
}

// Amounts is the estimate per hour type for one person.
type Amounts struct {
	ByBucket map[string]float64 `json:"byBucket"`
	Engaged  float64            `json:"engaged"`
	Stipend  float64            `json:"stipend"`
	Subtotal float64            `json:"subtotal"`
	Total    float64            `json:"total"`
	Note     string             `json:"note,omitempty"` // e.g. cap or floor applied
}

// Price computes the estimate from level 1 hours, level 2+ hours and engaged seconds per bucket.
func (c Config) Price(l1, l2, engaged engine.Hours) Amounts {
	a := Amounts{ByBucket: map[string]float64{}}
	for _, b := range engine.Buckets {
		a.ByBucket[b] += c.RoundSeconds(l1[b]) / 3600 * c.EffectiveRate(false, b)
		a.ByBucket[b] += c.RoundSeconds(l2[b]) / 3600 * c.EffectiveRate(true, b)
	}
	// Engaged time is charged at the level-1 engaged rate.
	a.Engaged = c.RoundSeconds(engaged.Total()) / 3600 * c.EngagedRate(false)
	a.Stipend = c.StipendPerPerson
	for _, v := range a.ByBucket {
		a.Subtotal += v
	}
	a.Subtotal += a.Engaged + a.Stipend
	a.Total = a.Subtotal
	if c.CapPerPerson > 0 && a.Total > c.CapPerPerson {
		a.Total, a.Note = c.CapPerPerson, "cap applied"
	}
	if c.FloorPerPerson > 0 && a.Total < c.FloorPerPerson {
		a.Total, a.Note = c.FloorPerPerson, "floor applied"
	}
	return a
}
