// Package export renders a report as CSV, JSON, XLSX, HTML and PDF.
package export

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/xuri/excelize/v2"

	"oncallcalc/internal/engine"
	"oncallcalc/internal/report"
)

// Options chosen on the Export step.
type Options struct {
	Title     string `json:"title"`
	Detail    bool   `json:"detail"` // day-by-day
	Rules     bool   `json:"rules"`  // print rules and rates
	Anonymize bool   `json:"anonymize"`
}

var bucketLabel = map[string]string{
	engine.Business: "Business", engine.NonBusiness: "Non-business", engine.Night: "Night",
	engine.Weekend: "Weekend", engine.WeekendNight: "Weekend night", engine.Holiday: "Holiday", engine.HolidayNight: "Holiday night",
}

// Label is the display name of an hour type.
func Label(b string) string { return bucketLabel[b] }

func h(sec float64) float64 { return float64(int64(sec/36+0.5)) / 100 } // hours, 2 decimals

func (o Options) name(r *report.Result, i int) string {
	if o.Anonymize {
		return fmt.Sprintf("Responder %d", i+1)
	}
	return r.People[i].Name
}

// Disclaimer is printed on every export.
func Disclaimer(r *report.Result) string {
	return fmt.Sprintf("Period boundaries use %s; hours are classified in each responder's local time.", r.Config.ReportTZ)
}

// RulesLines describes the rules and rates used, so a report can be audited.
func RulesLines(r *report.Result) []string {
	c := r.Config
	ru := c.Rules
	l := []string{
		fmt.Sprintf("Period: %s to %s (reporting time zone %s)", c.From, c.To, c.ReportTZ),
		fmt.Sprintf("Business hours: %s–%s on days %v; night: %s–%s; weekend days %v", ru.BusinessStart, ru.BusinessEnd, ru.BusinessDays, ru.NightStart, ru.NightEnd, ru.WeekendDays),
		fmt.Sprintf("Windows evaluated in: %s", map[bool]string{true: "a fixed time zone (" + ru.FixedTZ + ")", false: "each responder's own time zone"}[ru.TZMode == "fixed"]),
		fmt.Sprintf("Holidays: mode %s, split holiday night: %v; priority order: %s", ru.HolidayMode, ru.SplitHolidayNight, strings.Join(ru.Precedence, " > ")),
		fmt.Sprintf("Level 2+ overlap: %s", ru.Overlap),
		fmt.Sprintf("Engaged time included: %v (measured in on-call windows, attributed to the window in which the incident falls)", c.IncludeEngaged),
		fmt.Sprintf("Data fetched: %s; %d API calls, %d retries", r.Created.Format(time.RFC3339), r.Stats.Calls, r.Stats.Retries),
	}
	if c.Pricing.Enabled {
		p := c.Pricing
		l = append(l, fmt.Sprintf("Pricing style: %s, currency %s, combined hours: %s. ESTIMATE, NOT PAYROLL.", p.Style, p.Currency, p.Stack))
		for _, b := range engine.Buckets {
			l = append(l, fmt.Sprintf("  %s: %.2f per hour", bucketLabel[b], p.EffectiveRate(false, b)))
		}
		l = append(l, fmt.Sprintf("  Engaged: %.2f per hour", p.EngagedRate(false)))
		if p.StipendPerPerson > 0 {
			l = append(l, fmt.Sprintf("  Stipend per person: %.2f", p.StipendPerPerson))
		}
	}
	return l
}

func l2total(p report.PersonResult) float64 { return engine.Hours(p.L2).Total() }

// ---- CSV

// CSVSummary: one row per person, a column per hour type.
func CSVSummary(r *report.Result, o Options) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	head := []string{"person", "id", "time_zone"}
	for _, k := range engine.Buckets {
		head = append(head, k+"_hours")
	}
	head = append(head, "level1_total_hours", "level2plus_hours", "engaged_hours")
	if r.Config.Pricing.Enabled {
		for _, k := range engine.Buckets {
			head = append(head, k+"_estimate")
		}
		head = append(head, "engaged_estimate", "stipend", "total_estimate", "currency")
	}
	head = append(head, "incomplete")
	w.Write(head)
	for i, p := range r.People {
		row := []string{o.name(r, i), idOf(p, o), tzOf(p)}
		for _, k := range engine.Buckets {
			row = append(row, f2(h(p.L1[k])))
		}
		row = append(row, f2(h(engine.Hours(p.L1).Total())), f2(h(l2total(p))), f2(h(engine.Hours(p.Engaged).Total())))
		if p.Amounts != nil {
			for _, k := range engine.Buckets {
				row = append(row, f2(p.Amounts.ByBucket[k]))
			}
			row = append(row, f2(p.Amounts.Engaged), f2(p.Amounts.Stipend), f2(p.Amounts.Total), r.Config.Pricing.Currency)
		}
		row = append(row, strings.Join(p.Incomplete, "; "))
		w.Write(row)
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

// CSVLong: one row per person and hour type (payroll friendly).
func CSVLong(r *report.Result, o Options) ([]byte, error) {
	var b bytes.Buffer
	w := csv.NewWriter(&b)
	head := []string{"person", "id", "time_zone", "level", "hour_type", "hours"}
	if r.Config.Pricing.Enabled {
		head = append(head, "rate_per_hour", "estimate", "currency")
	}
	w.Write(head)
	for i, p := range r.People {
		emit := func(level, typ string, secs float64, lvl2 bool, engaged bool) {
			row := []string{o.name(r, i), idOf(p, o), tzOf(p), level, typ, f2(h(secs))}
			if p.Amounts != nil {
				rate := 0.0
				switch {
				case engaged:
					rate = r.Config.Pricing.EngagedRate(false)
				default:
					rate = r.Config.Pricing.EffectiveRate(lvl2, typ)
				}
				row = append(row, f2(rate), f2(r.Config.Pricing.RoundSeconds(secs)/3600*rate), r.Config.Pricing.Currency)
			}
			w.Write(row)
		}
		for _, k := range engine.Buckets {
			emit("1", k, p.L1[k], false, false)
		}
		for _, k := range engine.Buckets {
			if p.L2[k] > 0 {
				emit("2+", k, p.L2[k], true, false)
			}
		}
		emit("", "engaged", engine.Hours(p.Engaged).Total(), false, true)
	}
	w.Flush()
	return b.Bytes(), w.Error()
}

func f2(v float64) string { return fmt.Sprintf("%.2f", v) }
func idOf(p report.PersonResult, o Options) string {
	if o.Anonymize {
		return ""
	}
	return p.ID
}
func tzOf(p report.PersonResult) string {
	if p.TZOverride != "" {
		return p.TZOverride
	}
	return p.TimeZone
}

// ---- JSON

func JSON(r *report.Result, o Options) ([]byte, error) {
	cp := *r
	cp.People = append([]report.PersonResult(nil), r.People...)
	if o.Anonymize {
		for i := range cp.People {
			cp.People[i].Name = fmt.Sprintf("Responder %d", i+1)
			cp.People[i].ID = ""
		}
	}
	if !o.Detail {
		for i := range cp.People {
			cp.People[i].Days = nil
		}
	}
	return json.MarshalIndent(map[string]any{"disclaimer": Disclaimer(r), "rulesUsed": RulesLines(r), "report": cp}, "", "  ")
}

// ---- XLSX

func XLSX(r *report.Result, o Options) ([]byte, error) {
	f := excelize.NewFile()
	sum := "Summary"
	f.SetSheetName("Sheet1", sum)
	head := []any{"Person", "Time zone"}
	for _, k := range engine.Buckets {
		head = append(head, bucketLabel[k])
	}
	head = append(head, "Level 1 total", "Level 2+", "Engaged")
	f.SetSheetRow(sum, "A1", &head)
	for i, p := range r.People {
		row := []any{o.name(r, i), tzOf(p)}
		for _, k := range engine.Buckets {
			row = append(row, h(p.L1[k]))
		}
		row = append(row, h(engine.Hours(p.L1).Total()), h(l2total(p)), h(engine.Hours(p.Engaged).Total()))
		cell, _ := excelize.CoordinatesToCellName(1, i+2)
		f.SetSheetRow(sum, cell, &row)
	}
	n := len(r.People) + 3
	f.SetCellValue(sum, fmt.Sprintf("A%d", n), Disclaimer(r))
	if r.Config.Pricing.Enabled {
		est := "Estimate"
		f.NewSheet(est)
		eh := []any{"Person"}
		for _, k := range engine.Buckets {
			eh = append(eh, bucketLabel[k])
		}
		eh = append(eh, "Engaged", "Stipend", "Total estimate ("+r.Config.Pricing.Currency+")", "Note")
		f.SetSheetRow(est, "A1", &eh)
		for i, p := range r.People {
			row := []any{o.name(r, i)}
			for _, k := range engine.Buckets {
				row = append(row, round2(p.Amounts.ByBucket[k]))
			}
			row = append(row, round2(p.Amounts.Engaged), round2(p.Amounts.Stipend), round2(p.Amounts.Total), p.Amounts.Note)
			cell, _ := excelize.CoordinatesToCellName(1, i+2)
			f.SetSheetRow(est, cell, &row)
		}
		f.SetCellValue(est, fmt.Sprintf("A%d", len(r.People)+3), "Estimate, not payroll. "+Disclaimer(r))
	}
	if o.Detail {
		d := "Detail"
		f.NewSheet(d)
		dh := []any{"Person", "Date", "Day"}
		for _, k := range engine.Buckets {
			dh = append(dh, bucketLabel[k])
		}
		dh = append(dh, "Level 2+", "Engaged", "Note")
		f.SetSheetRow(d, "A1", &dh)
		row := 2
		for i, p := range r.People {
			for _, dd := range p.Days {
				v := []any{o.name(r, i), dd.Date, dd.DayType}
				for _, k := range engine.Buckets {
					v = append(v, h(dd.Hours[k]))
				}
				v = append(v, h(dd.L2), h(dd.Engaged), dd.Note)
				cell, _ := excelize.CoordinatesToCellName(1, row)
				f.SetSheetRow(d, cell, &v)
				row++
			}
		}
	}
	if o.Rules {
		ru := "Rules used"
		f.NewSheet(ru)
		for i, l := range RulesLines(r) {
			f.SetCellValue(ru, fmt.Sprintf("A%d", i+1), l)
		}
	}
	var b bytes.Buffer
	if err := f.Write(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// ---- HTML

func HTML(r *report.Result, o Options) ([]byte, error) {
	var b strings.Builder
	title := html.EscapeString(orDefault(o.Title, "On-call report"))
	fmt.Fprintf(&b, `<!doctype html><html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1"><title>%s</title><style>
body{font-family:"Helvetica Neue",Arial,sans-serif;color:#171817;margin:0;background:#fff}
header{background:#005A2F;color:#fff;padding:20px 32px}h1{font-weight:300;margin:0;font-size:26px}
main{padding:24px 32px;max-width:1200px;margin:auto}h2{font-weight:300;border-bottom:2px solid #06AC38;padding-bottom:4px}
table{border-collapse:collapse;width:100%%;font-size:13px;margin:12px 0}th,td{padding:6px 10px;border-bottom:1px solid #E6E6E6;text-align:right}
th:first-child,td:first-child{text-align:left}th{font-size:11px;text-transform:uppercase;color:#5F655F}
tfoot td{font-weight:700;border-top:2px solid #171817}.dis{color:#F82431;font-size:11px}.rules{font-size:12px;color:#444}
@media print{header{-webkit-print-color-adjust:exact;print-color-adjust:exact}}
</style></head><body><header><h1>%s</h1><div>%s to %s</div></header><main>`, title, title, r.Config.From, r.Config.To)
	b.WriteString("<h2>Hours by person</h2>" + tableHours(r, o))
	if r.Config.Pricing.Enabled {
		b.WriteString("<h2>Estimate by person</h2>" + tableEstimate(r, o))
		b.WriteString(`<p class="dis">Estimate, not payroll.</p>`)
	}
	fmt.Fprintf(&b, `<p class="dis">%s</p>`, html.EscapeString(Disclaimer(r)))
	if inc := incomplete(r, o); len(inc) > 0 {
		b.WriteString("<h2>Incomplete data</h2><ul>")
		for _, s := range inc {
			b.WriteString("<li>" + html.EscapeString(s) + "</li>")
		}
		b.WriteString("</ul>")
	}
	if o.Detail {
		b.WriteString("<h2>Day by day</h2>")
		for i, p := range r.People {
			fmt.Fprintf(&b, "<h3>%s</h3><table><thead><tr><th>Date</th><th>Day</th>", html.EscapeString(o.name(r, i)))
			for _, k := range engine.Buckets {
				fmt.Fprintf(&b, "<th>%s</th>", bucketLabel[k])
			}
			b.WriteString("<th>Level 2+</th><th>Engaged</th><th>Note</th></tr></thead><tbody>")
			for _, d := range p.Days {
				fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td>", d.Date, d.DayType)
				for _, k := range engine.Buckets {
					fmt.Fprintf(&b, "<td>%.2f</td>", h(d.Hours[k]))
				}
				fmt.Fprintf(&b, "<td>%.2f</td><td>%.2f</td><td>%s</td></tr>", h(d.L2), h(d.Engaged), html.EscapeString(d.Note))
			}
			b.WriteString("</tbody></table>")
		}
	}
	if o.Rules {
		b.WriteString(`<h2>Rules used</h2><div class="rules">`)
		for _, l := range RulesLines(r) {
			b.WriteString(html.EscapeString(l) + "<br>")
		}
		b.WriteString("</div>")
	}
	b.WriteString("</main></body></html>")
	return []byte(b.String()), nil
}

func orDefault(s, d string) string {
	if strings.TrimSpace(s) == "" {
		return d
	}
	return s
}

func tableHours(r *report.Result, o Options) string {
	var b strings.Builder
	b.WriteString("<table><thead><tr><th>Person</th><th>Time zone</th>")
	for _, k := range engine.Buckets {
		fmt.Fprintf(&b, "<th>%s</th>", bucketLabel[k])
	}
	b.WriteString("<th>Level 1 total</th><th>Level 2+</th><th>Engaged</th></tr></thead><tbody>")
	tot := map[string]float64{}
	var t1, t2, te float64
	for i, p := range r.People {
		fmt.Fprintf(&b, "<tr><td>%s</td><td>%s</td>", html.EscapeString(o.name(r, i)), tzOf(p))
		for _, k := range engine.Buckets {
			fmt.Fprintf(&b, "<td>%.2f</td>", h(p.L1[k]))
			tot[k] += p.L1[k]
		}
		a, c, e := engine.Hours(p.L1).Total(), l2total(p), engine.Hours(p.Engaged).Total()
		t1, t2, te = t1+a, t2+c, te+e
		fmt.Fprintf(&b, "<td><b>%.2f</b></td><td>%.2f</td><td>%.2f</td></tr>", h(a), h(c), h(e))
	}
	b.WriteString("</tbody><tfoot><tr><td>Total</td><td></td>")
	for _, k := range engine.Buckets {
		fmt.Fprintf(&b, "<td>%.2f</td>", h(tot[k]))
	}
	fmt.Fprintf(&b, "<td>%.2f</td><td>%.2f</td><td>%.2f</td></tr></tfoot></table>", h(t1), h(t2), h(te))
	return b.String()
}

func tableEstimate(r *report.Result, o Options) string {
	var b strings.Builder
	cur := r.Config.Pricing.Currency
	b.WriteString("<table><thead><tr><th>Person</th>")
	for _, k := range engine.Buckets {
		fmt.Fprintf(&b, "<th>%s</th>", bucketLabel[k])
	}
	fmt.Fprintf(&b, "<th>Engaged</th><th>Stipend</th><th>Total estimate (%s)</th></tr></thead><tbody>", cur)
	tot := map[string]float64{}
	var te, ts, tt float64
	for i, p := range r.People {
		fmt.Fprintf(&b, "<tr><td>%s</td>", html.EscapeString(o.name(r, i)))
		for _, k := range engine.Buckets {
			fmt.Fprintf(&b, "<td>%.2f</td>", p.Amounts.ByBucket[k])
			tot[k] += p.Amounts.ByBucket[k]
		}
		te, ts, tt = te+p.Amounts.Engaged, ts+p.Amounts.Stipend, tt+p.Amounts.Total
		fmt.Fprintf(&b, "<td>%.2f</td><td>%.2f</td><td><b>%.2f</b></td></tr>", p.Amounts.Engaged, p.Amounts.Stipend, p.Amounts.Total)
	}
	b.WriteString("</tbody><tfoot><tr><td>Total</td>")
	for _, k := range engine.Buckets {
		fmt.Fprintf(&b, "<td>%.2f</td>", tot[k])
	}
	fmt.Fprintf(&b, "<td>%.2f</td><td>%.2f</td><td>%.2f</td></tr></tfoot></table>", te, ts, tt)
	return b.String()
}

func incomplete(r *report.Result, o Options) []string {
	var out []string
	for i, p := range r.People {
		for _, s := range p.Incomplete {
			out = append(out, o.name(r, i)+": "+s)
		}
	}
	return append(out, r.Warnings...)
}

// ---- PDF

func PDF(r *report.Result, o Options) ([]byte, error) {
	p := fpdf.New("L", "mm", "A4", "")
	p.SetMargins(10, 12, 10)
	p.SetAutoPageBreak(true, 12)
	tr := p.UnicodeTranslatorFromDescriptor("")
	p.AddPage()
	p.SetFillColor(0, 90, 47)
	p.Rect(0, 0, 297, 24, "F")
	p.SetTextColor(255, 255, 255)
	p.SetFont("Helvetica", "", 20)
	p.SetXY(10, 6)
	p.Cell(0, 8, tr(orDefault(o.Title, "On-call report")))
	p.SetFont("Helvetica", "", 10)
	p.SetXY(10, 15)
	p.Cell(0, 6, fmt.Sprintf("%s to %s", r.Config.From, r.Config.To))
	p.SetTextColor(23, 24, 23)
	p.SetY(30)

	table := func(head []string, rows [][]string, foot []string) {
		w := (297.0 - 20) / float64(len(head))
		first := w * 1.6
		other := (297.0 - 20 - first) / float64(len(head)-1)
		p.SetFont("Helvetica", "B", 7.5)
		p.SetFillColor(230, 230, 230)
		for i, hd := range head {
			cw := other
			if i == 0 {
				cw = first
			}
			p.CellFormat(cw, 6, tr(hd), "B", 0, map[bool]string{true: "L", false: "R"}[i == 0], true, 0, "")
		}
		p.Ln(-1)
		p.SetFont("Helvetica", "", 8)
		draw := func(r []string, bold bool) {
			if bold {
				p.SetFont("Helvetica", "B", 8)
			}
			for i, c := range r {
				cw := other
				if i == 0 {
					cw = first
				}
				p.CellFormat(cw, 5.5, tr(c), "B", 0, map[bool]string{true: "L", false: "R"}[i == 0], false, 0, "")
			}
			p.Ln(-1)
		}
		for _, r := range rows {
			draw(r, false)
		}
		if foot != nil {
			draw(foot, true)
		}
		p.Ln(3)
	}

	p.SetFont("Helvetica", "B", 12)
	p.Cell(0, 7, "Hours by person")
	p.Ln(8)
	head := []string{"Person"}
	for _, k := range engine.Buckets {
		head = append(head, bucketLabel[k])
	}
	head = append(head, "L1 total", "Level 2+", "Engaged")
	var rows [][]string
	tot := map[string]float64{}
	var t1, t2, te float64
	for i, pr := range r.People {
		row := []string{o.name(r, i)}
		for _, k := range engine.Buckets {
			row = append(row, f2(h(pr.L1[k])))
			tot[k] += pr.L1[k]
		}
		a, c, e := engine.Hours(pr.L1).Total(), l2total(pr), engine.Hours(pr.Engaged).Total()
		t1, t2, te = t1+a, t2+c, te+e
		rows = append(rows, append(row, f2(h(a)), f2(h(c)), f2(h(e))))
	}
	foot := []string{"Total"}
	for _, k := range engine.Buckets {
		foot = append(foot, f2(h(tot[k])))
	}
	table(head, rows, append(foot, f2(h(t1)), f2(h(t2)), f2(h(te))))

	if r.Config.Pricing.Enabled {
		p.SetFont("Helvetica", "B", 12)
		p.Cell(0, 7, "Estimate by person ("+r.Config.Pricing.Currency+")")
		p.Ln(8)
		eh := []string{"Person"}
		for _, k := range engine.Buckets {
			eh = append(eh, bucketLabel[k])
		}
		eh = append(eh, "Engaged", "Stipend", "Total estimate")
		var er [][]string
		et := map[string]float64{}
		var ee, es, ett float64
		for i, pr := range r.People {
			row := []string{o.name(r, i)}
			for _, k := range engine.Buckets {
				row = append(row, f2(pr.Amounts.ByBucket[k]))
				et[k] += pr.Amounts.ByBucket[k]
			}
			ee, es, ett = ee+pr.Amounts.Engaged, es+pr.Amounts.Stipend, ett+pr.Amounts.Total
			er = append(er, append(row, f2(pr.Amounts.Engaged), f2(pr.Amounts.Stipend), f2(pr.Amounts.Total)))
		}
		ef := []string{"Total"}
		for _, k := range engine.Buckets {
			ef = append(ef, f2(et[k]))
		}
		table(eh, er, append(ef, f2(ee), f2(es), f2(ett)))
		p.SetTextColor(248, 36, 49)
		p.SetFont("Helvetica", "", 7)
		p.Cell(0, 4, "Estimate, not payroll.")
		p.Ln(5)
	}
	p.SetTextColor(248, 36, 49)
	p.SetFont("Helvetica", "", 7)
	p.Cell(0, 4, tr(Disclaimer(r)))
	p.Ln(6)
	p.SetTextColor(23, 24, 23)
	if inc := incomplete(r, o); len(inc) > 0 {
		p.SetFont("Helvetica", "B", 10)
		p.Cell(0, 6, "Incomplete data")
		p.Ln(6)
		p.SetFont("Helvetica", "", 8)
		for _, s := range inc {
			p.MultiCell(0, 4, tr("- "+s), "", "L", false)
		}
		p.Ln(2)
	}
	if o.Rules {
		p.SetFont("Helvetica", "B", 10)
		p.Cell(0, 6, "Rules used")
		p.Ln(6)
		p.SetFont("Helvetica", "", 8)
		for _, l := range RulesLines(r) {
			p.MultiCell(0, 4, tr(l), "", "L", false)
		}
	}
	var b bytes.Buffer
	if err := p.Output(&b); err != nil {
		return nil, err
	}
	return b.Bytes(), nil
}

// Render produces a file in the requested format: csv, csv_detail, json, xlsx, html or pdf.
func Render(r *report.Result, format string, o Options) (data []byte, filename, ctype string, err error) {
	base := "oncall-report-" + r.Config.From + "_" + r.Config.To
	switch format {
	case "csv":
		data, err = CSVSummary(r, o)
		return data, base + ".csv", "text/csv", err
	case "csv_detail":
		data, err = CSVLong(r, o)
		return data, base + "-by-hour-type.csv", "text/csv", err
	case "json":
		data, err = JSON(r, o)
		return data, base + ".json", "application/json", err
	case "xlsx":
		data, err = XLSX(r, o)
		return data, base + ".xlsx", "application/vnd.openxmlformats-officedocument.spreadsheetml.sheet", err
	case "html":
		data, err = HTML(r, o)
		return data, base + ".html", "text/html", err
	case "pdf":
		data, err = PDF(r, o)
		return data, base + ".pdf", "application/pdf", err
	}
	return nil, "", "", fmt.Errorf("unknown format %q", format)
}
