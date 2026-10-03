package pricing

import (
	"math"
	"testing"

	"oncallcalc/internal/engine"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// Customer X: base 5, +25% outside business hours, +50% at night.
func TestCustomerX(t *testing.T) {
	c := Default()
	if !near(c.EffectiveRate(false, engine.Business), 5) ||
		!near(c.EffectiveRate(false, engine.NonBusiness), 6.25) ||
		!near(c.EffectiveRate(false, engine.Night), 7.5) {
		t.Fatal("customer X rates wrong")
	}
	l1 := engine.Hours{engine.Business: 8 * 3600, engine.NonBusiness: 4 * 3600, engine.Night: 2 * 3600}
	a := c.Price(l1, nil, nil)
	if !near(a.Total, 8*5+4*6.25+2*7.5) {
		t.Fatalf("total %v", a.Total)
	}
}

func TestStacking(t *testing.T) {
	c := Default() // weekend 25 + night 50
	if !near(c.EffectiveRate(false, engine.WeekendNight), 5*1.75) {
		t.Fatal("add")
	}
	c.Stack = "max"
	if !near(c.EffectiveRate(false, engine.WeekendNight), 5*1.5) {
		t.Fatal("max")
	}
	c.Stack = "custom"
	if !near(c.EffectiveRate(false, engine.WeekendNight), 5*1.75) { // custom value = Surcharge[weekend_night] (75)
		t.Fatal("custom")
	}
}

func TestRateStyleCapFloorRoundingLevel2(t *testing.T) {
	c := Config{Enabled: true, Style: "rate", Level2: "separate", RoundMode: "up", RoundMinutes: 15,
		L1: RateSet{Rate: map[string]float64{engine.Business: 10}, EngagedRate: 20},
		L2: RateSet{Rate: map[string]float64{engine.Business: 4}}, CapPerPerson: 100}
	a := c.Price(engine.Hours{engine.Business: 3600 + 60}, engine.Hours{engine.Business: 3600}, engine.Hours{engine.Business: 1800})
	// 1h01m rounds up to 1h15m => 12.5; level 2: 4; engaged 0.5h*20=10
	if !near(a.Total, 12.5+4+10) {
		t.Fatalf("%v", a)
	}
	big := c.Price(engine.Hours{engine.Business: 100 * 3600}, nil, nil)
	if big.Total != 100 || big.Note != "cap applied" {
		t.Fatalf("%v", big)
	}
}
