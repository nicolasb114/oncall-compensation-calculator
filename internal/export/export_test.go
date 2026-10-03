package export

import (
	"bytes"
	"strings"
	"testing"
	"time"

	"oncallcalc/internal/engine"
	"oncallcalc/internal/pricing"
	"oncallcalc/internal/report"
)

func sample() *report.Result {
	pc := pricing.Default()
	l1 := map[string]float64{engine.Business: 8 * 3600, engine.Night: 4 * 3600}
	a := pc.Price(l1, nil, map[string]float64{engine.Business: 1800})
	return &report.Result{
		Created: time.Now(),
		Config:  report.Config{From: "2026-09-01", To: "2026-09-30", ReportTZ: "America/New_York", Rules: engine.DefaultRules(), Pricing: pc, IncludeEngaged: true},
		People: []report.PersonResult{{Person: report.Person{ID: "P1", Name: "Ana Torres", TimeZone: "America/Lima"},
			L1: l1, L2: map[string]float64{}, Engaged: map[string]float64{engine.Business: 1800}, Amounts: &a,
			Days: []report.DayRow{{Date: "2026-09-01", DayType: "Tue", Hours: l1}}}},
	}
}

func TestAllFormats(t *testing.T) {
	r, o := sample(), Options{Title: "T", Detail: true, Rules: true}
	for name, fn := range map[string]func(*report.Result, Options) ([]byte, error){
		"csv": CSVSummary, "csvlong": CSVLong, "json": JSON, "xlsx": XLSX, "html": HTML, "pdf": PDF} {
		b, err := fn(r, o)
		if err != nil || len(b) < 50 {
			t.Fatalf("%s: %v len=%d", name, err, len(b))
		}
		if name == "pdf" && !bytes.HasPrefix(b, []byte("%PDF")) {
			t.Fatal("not a pdf")
		}
		if name == "xlsx" && !bytes.HasPrefix(b, []byte("PK")) {
			t.Fatal("not an xlsx")
		}
		if name == "html" && !strings.Contains(string(b), "classified in each responder") {
			t.Fatal("disclaimer missing from html")
		}
	}
}

func TestAnonymizeAndHoursOnly(t *testing.T) {
	r := sample()
	o := Options{Anonymize: true, Detail: true}
	for _, fn := range []func(*report.Result, Options) ([]byte, error){CSVSummary, CSVLong, JSON, HTML} {
		b, _ := fn(r, o)
		if strings.Contains(string(b), "Ana Torres") || strings.Contains(string(b), "\"P1\"") {
			t.Fatal("name or id leaked")
		}
	}
	r.Config.Pricing.Enabled = false
	r.People[0].Amounts = nil
	b, err := CSVSummary(r, Options{})
	if err != nil || strings.Contains(string(b), "estimate") {
		t.Fatalf("hours-only csv contains money: %s", b)
	}
	if _, err := PDF(r, Options{}); err != nil {
		t.Fatal(err)
	}
}
