package report

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"oncallcalc/internal/engine"
)

// Holidays provides public-holiday dates per country, fetched on demand from the free
// Nager.Date service (only the year and country code are sent) and cached on disk.
type Holidays struct {
	CacheDir string
	BaseURL  string
	HTTP     *http.Client
	mu       sync.Mutex
	mem      map[string][]string
}

func NewHolidays(cacheDir string) *Holidays {
	return &Holidays{CacheDir: cacheDir, BaseURL: "https://date.nager.at/api/v3/PublicHolidays", HTTP: &http.Client{Timeout: 15 * time.Second}, mem: map[string][]string{}}
}

// Dates returns YYYY-MM-DD public holidays for a country and year.
func (h *Holidays) Dates(ctx context.Context, country string, year int) ([]string, error) {
	country = strings.ToUpper(strings.TrimSpace(country))
	if country == "" {
		return nil, nil
	}
	key := fmt.Sprintf("%s-%d", country, year)
	h.mu.Lock()
	if v, ok := h.mem[key]; ok {
		h.mu.Unlock()
		return v, nil
	}
	h.mu.Unlock()
	file := filepath.Join(h.CacheDir, "holidays-"+key+".json")
	var dates []string
	if b, err := os.ReadFile(file); err == nil && json.Unmarshal(b, &dates) == nil {
		h.mu.Lock()
		h.mem[key] = dates
		h.mu.Unlock()
		return dates, nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", fmt.Sprintf("%s/%d/%s", h.BaseURL, year, country), nil)
	resp, err := h.HTTP.Do(req)
	if err != nil {
		return nil, fmt.Errorf("could not load %s holidays for %d (add dates manually instead): %w", country, year, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("no built-in holiday list for %s %d (HTTP %d); add dates manually", country, year, resp.StatusCode)
	}
	var rows []struct {
		Date   string `json:"date"`
		Global bool   `json:"global"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rows); err != nil {
		return nil, err
	}
	for _, r := range rows {
		dates = append(dates, r.Date)
	}
	if h.CacheDir != "" {
		_ = os.MkdirAll(h.CacheDir, 0o755)
		if b, err := json.Marshal(dates); err == nil {
			_ = os.WriteFile(file, b, 0o644)
		}
	}
	h.mu.Lock()
	h.mem[key] = dates
	h.mu.Unlock()
	return dates, nil
}

// Set builds the holiday set for a country across the years touched by the period, plus manual dates.
func (h *Holidays) Set(ctx context.Context, country string, from, to time.Time, manual []string) (engine.HolidaySet, []string) {
	set := engine.HolidaySet{}
	var warns []string
	for _, d := range manual {
		set[strings.TrimSpace(d)] = true
	}
	for y := from.Year() - 0; y <= to.Year(); y++ {
		ds, err := h.Dates(ctx, country, y)
		if err != nil {
			warns = append(warns, err.Error())
			continue
		}
		for _, d := range ds {
			set[d] = true
		}
	}
	return set, warns
}

// CountryForTZ guesses a country code from an IANA zone (people can override it).
func CountryForTZ(tz string) string {
	if c, ok := tzCountry[tz]; ok {
		return c
	}
	return ""
}

var tzCountry = map[string]string{
	"America/New_York": "US", "America/Chicago": "US", "America/Denver": "US", "America/Los_Angeles": "US", "America/Phoenix": "US",
	"America/Anchorage": "US", "Pacific/Honolulu": "US", "America/Detroit": "US", "America/Boise": "US",
	"America/Toronto": "CA", "America/Vancouver": "CA", "America/Edmonton": "CA", "America/Winnipeg": "CA", "America/Halifax": "CA",
	"America/Mexico_City": "MX", "America/Bogota": "CO", "America/Lima": "PE", "America/Santiago": "CL",
	"America/Argentina/Buenos_Aires": "AR", "America/Sao_Paulo": "BR", "America/Caracas": "VE", "America/Montevideo": "UY",
	"America/Guayaquil": "EC", "America/La_Paz": "BO", "America/Panama": "PA", "America/Costa_Rica": "CR",
	"Europe/London": "GB", "Europe/Dublin": "IE", "Europe/Paris": "FR", "Europe/Berlin": "DE", "Europe/Madrid": "ES",
	"Europe/Lisbon": "PT", "Europe/Rome": "IT", "Europe/Amsterdam": "NL", "Europe/Brussels": "BE", "Europe/Zurich": "CH",
	"Europe/Vienna": "AT", "Europe/Stockholm": "SE", "Europe/Oslo": "NO", "Europe/Copenhagen": "DK", "Europe/Helsinki": "FI",
	"Europe/Warsaw": "PL", "Europe/Prague": "CZ", "Europe/Budapest": "HU", "Europe/Bucharest": "RO", "Europe/Athens": "GR",
	"Europe/Sofia": "BG", "Europe/Kiev": "UA", "Europe/Kyiv": "UA", "Europe/Istanbul": "TR", "Europe/Moscow": "RU",
	"Africa/Cairo": "EG", "Africa/Johannesburg": "ZA", "Africa/Lagos": "NG", "Africa/Nairobi": "KE", "Africa/Casablanca": "MA",
	"Asia/Dubai": "AE", "Asia/Riyadh": "SA", "Asia/Jerusalem": "IL", "Asia/Karachi": "PK", "Asia/Kolkata": "IN", "Asia/Calcutta": "IN",
	"Asia/Dhaka": "BD", "Asia/Bangkok": "TH", "Asia/Jakarta": "ID", "Asia/Manila": "PH", "Asia/Singapore": "SG",
	"Asia/Kuala_Lumpur": "MY", "Asia/Ho_Chi_Minh": "VN", "Asia/Hong_Kong": "HK", "Asia/Shanghai": "CN", "Asia/Taipei": "TW",
	"Asia/Seoul": "KR", "Asia/Tokyo": "JP", "Australia/Sydney": "AU", "Australia/Melbourne": "AU", "Australia/Brisbane": "AU",
	"Australia/Perth": "AU", "Australia/Adelaide": "AU", "Pacific/Auckland": "NZ",
}
