package report

import (
	"context"
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"testing"

	"oncallcalc/internal/engine"
	"oncallcalc/internal/pd"
	"oncallcalc/internal/pricing"
)

func TestRunEndToEnd(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/oncalls":
			json.NewEncoder(w).Encode(map[string]any{"oncalls": []any{
				map[string]any{"escalation_level": 1, "start": "2026-09-14T09:00:00Z", "end": "2026-09-15T09:00:00Z", "user": map[string]string{"id": "U1"}},
				// overlapping duplicate on another policy must not double count
				map[string]any{"escalation_level": 1, "start": "2026-09-14T12:00:00Z", "end": "2026-09-14T13:00:00Z", "user": map[string]string{"id": "U1"}},
				map[string]any{"escalation_level": 2, "start": "2026-09-16T00:00:00Z", "end": "2026-09-16T04:00:00Z", "user": map[string]string{"id": "U1"}},
			}, "more": false})
		default: // analytics
			json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"responder_id": "U1", "total_engaged_seconds": 600}}, "more": false})
		}
	}))
	defer srv.Close()
	cl := pd.New("t", "us", pd.Settings{DelayMS: 0, MaxRetries: 1, TimeoutS: 5})
	cl.BaseURL = srv.URL

	cfg := Config{From: "2026-09-14", To: "2026-09-16", ReportTZ: "UTC", IncludeEngaged: true,
		People:         []Person{{ID: "U1", Name: "Ana", TimeZone: "UTC"}},
		Rules:          engine.DefaultRules(),
		HolidayCountry: "manual",
		Pricing:        pricing.Default()}
	res, err := Run(context.Background(), cl, NewHolidays(t.TempDir()), cfg, func(string, int, int) {})
	if err != nil {
		t.Fatal(err)
	}
	p := res.People[0]
	h := func(b string) float64 { return p.L1[b] / 3600 }
	if math.Abs(h(engine.Business)-8) > 1e-9 || math.Abs(h(engine.NonBusiness)-8) > 1e-9 || math.Abs(h(engine.Night)-8) > 1e-9 {
		t.Fatalf("L1 %v", p.L1)
	}
	var l2 float64
	for _, v := range p.L2 {
		l2 += v
	}
	if l2/3600 != 4 {
		t.Fatalf("L2 hours %v", l2/3600)
	}
	var eng float64
	for _, v := range p.Engaged {
		eng += v
	}
	if eng == 0 || p.Amounts == nil || p.Amounts.Total <= 0 {
		t.Fatalf("engaged %v amounts %v", eng, p.Amounts)
	}
	if len(p.Days) == 0 {
		t.Fatal("no day rows")
	}
}
