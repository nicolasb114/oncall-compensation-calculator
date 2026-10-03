// Package server is the local web UI backend. It listens on localhost only and requires a
// per-session access token.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io/fs"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"oncallcalc/internal/engine"
	"oncallcalc/internal/pd"
	"oncallcalc/internal/pricing"
	"oncallcalc/internal/report"
	"oncallcalc/internal/store"
)

// Server holds the app state.
type Server struct {
	Store *store.Store
	Hol   *report.Holidays
	Token string
	Web   fs.FS

	mu    sync.Mutex
	jobs  map[string]*Job
	metas map[string]*Meta
}

// Meta is the cached filter data of a connection.
type Meta struct {
	Teams, Schedules, Policies []pd.Ref
	Services                   []pd.Service
	Users                      []pd.User
}

// New creates a server.
func New(st *store.Store, web fs.FS) *Server {
	b := make([]byte, 16)
	rand.Read(b)
	return &Server{Store: st, Hol: report.NewHolidays(st.Dir), Token: hex.EncodeToString(b), Web: web, jobs: map[string]*Job{}, metas: map[string]*Meta{}}
}

func (s *Server) client(connID string, set pd.Settings) (*pd.Client, error) {
	conns, err := s.Store.Connections()
	if err != nil {
		return nil, err
	}
	for _, c := range conns {
		if c.ID == connID {
			tok, err := s.Store.Token(c.ID)
			if err != nil {
				return nil, fmt.Errorf("the API key for %q is missing from the credential store; add the connection again", c.Name)
			}
			return pd.New(tok, c.Region, set), nil
		}
	}
	return nil, fmt.Errorf("unknown connection")
}

// Handler returns the HTTP handler.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /api/state", s.api(s.state))
	mux.HandleFunc("POST /api/connections", s.api(s.addConn))
	mux.HandleFunc("DELETE /api/connections/{id}", s.api(s.delConn))
	mux.HandleFunc("GET /api/meta", s.api(s.meta))
	mux.HandleFunc("POST /api/people", s.api(s.people))
	mux.HandleFunc("POST /api/reports", s.api(s.saveReport))
	mux.HandleFunc("DELETE /api/reports/{id}", s.api(s.delReport))
	mux.HandleFunc("POST /api/run", s.api(s.run))
	mux.HandleFunc("GET /api/jobs/{id}", s.api(s.jobStatus))
	mux.HandleFunc("POST /api/jobs/{id}/{action}", s.api(s.jobAction))
	mux.HandleFunc("GET /api/jobs/{id}/export", s.exportJob)
	mux.HandleFunc("GET /api/holidays", s.api(s.holidays))
	mux.Handle("GET /", http.FileServer(http.FS(s.Web)))
	return s.guard(mux)
}

// guard enforces localhost Host headers (DNS-rebinding protection) and the session token.
func (s *Server) guard(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		host, _, err := net.SplitHostPort(r.Host)
		if err != nil {
			host = r.Host
		}
		if host != "127.0.0.1" && host != "localhost" && host != "[::1]" && host != "::1" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		if t := r.URL.Query().Get("t"); t != "" && t == s.Token {
			http.SetCookie(w, &http.Cookie{Name: "oc_session", Value: s.Token, Path: "/", HttpOnly: true, SameSite: http.SameSiteStrictMode})
			if !strings.HasPrefix(r.URL.Path, "/api/") {
				http.Redirect(w, r, r.URL.Path, http.StatusFound)
				return
			}
		}
		if c, err := r.Cookie("oc_session"); err != nil || c.Value != s.Token {
			if !(r.URL.Query().Get("t") == s.Token) {
				if strings.HasPrefix(r.URL.Path, "/api/") {
					http.Error(w, "missing session; open the link printed by the application", http.StatusUnauthorized)
					return
				}
				// Static page is harmless without a session, but it can't talk to the API.
			}
		}
		if r.Method != http.MethodGet && r.Header.Get("X-Requested-With") != "oncall-ui" {
			http.Error(w, "forbidden", http.StatusForbidden)
			return
		}
		next.ServeHTTP(w, r)
	})
}

type handler func(r *http.Request) (any, int, error)

func (s *Server) api(h handler) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		v, code, err := h(r)
		w.Header().Set("Content-Type", "application/json")
		if err != nil {
			if code == 0 {
				code = http.StatusBadRequest
			}
			w.WriteHeader(code)
			json.NewEncoder(w).Encode(map[string]string{"error": friendly(err)})
			return
		}
		json.NewEncoder(w).Encode(v)
	}
}

// friendly turns API errors into plain-language messages that say what to do next.
func friendly(err error) string {
	if pd.IsEntitlement(err) {
		return "PagerDuty refused the request (access denied). Check that the API key is valid and that your plan includes access to this data (for engaged time: Analytics). Details: " + err.Error()
	}
	return err.Error()
}

func decode(r *http.Request, v any) error {
	d := json.NewDecoder(http.MaxBytesReader(nil, r.Body, 8<<20))
	return d.Decode(v)
}

// ---- handlers

func (s *Server) state(r *http.Request) (any, int, error) {
	cs, err := s.Store.Connections()
	if err != nil {
		return nil, 500, err
	}
	rs, err := s.Store.Reports()
	if err != nil {
		return nil, 500, err
	}
	pc := pricing.Default()
	return map[string]any{
		"connections": cs, "reports": rs,
		"defaults": map[string]any{"rules": engine.DefaultRules(), "pricing": pc, "settings": pd.DefaultSettings()},
		"buckets":  engine.Buckets,
	}, 0, nil
}

func (s *Server) addConn(r *http.Request) (any, int, error) {
	var in struct{ Name, Region, Token string }
	if err := decode(r, &in); err != nil {
		return nil, 0, err
	}
	in.Token = strings.TrimSpace(in.Token)
	if in.Token == "" || strings.TrimSpace(in.Name) == "" {
		return nil, 0, fmt.Errorf("enter a name and an API key")
	}
	if in.Region != "eu" {
		in.Region = "us"
	}
	ctx, cancel := context.WithTimeout(r.Context(), 60*time.Second)
	defer cancel()
	cl := pd.New(in.Token, in.Region, pd.Settings{DelayMS: 0, MaxRetries: 1, TimeoutS: 20})
	ab, err := cl.Validate(ctx)
	if err != nil {
		if pd.IsEntitlement(err) {
			return nil, 0, fmt.Errorf("PagerDuty rejected this API key. Check that you copied it fully, that the region (US/EU) is right, and that the key is still active")
		}
		return nil, 0, err
	}
	c, err := s.Store.AddConnection(in.Name, in.Region, in.Token, pd.HasAnalytics(ab))
	if err != nil {
		return nil, 500, err
	}
	return c, 0, nil
}

func (s *Server) delConn(r *http.Request) (any, int, error) {
	if err := s.Store.DeleteConnection(r.PathValue("id")); err != nil {
		return nil, 500, err
	}
	s.mu.Lock()
	delete(s.metas, r.PathValue("id"))
	s.mu.Unlock()
	return map[string]bool{"ok": true}, 0, nil
}

func (s *Server) meta(r *http.Request) (any, int, error) {
	m, err := s.getMeta(r.Context(), r.URL.Query().Get("conn"), r.URL.Query().Get("refresh") != "")
	if err != nil {
		return nil, 0, err
	}
	return m, 0, nil
}

func (s *Server) getMeta(ctx context.Context, id string, refresh bool) (*Meta, error) {
	s.mu.Lock()
	m := s.metas[id]
	s.mu.Unlock()
	if m != nil && !refresh {
		return m, nil
	}
	cl, err := s.client(id, pd.DefaultSettings())
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Minute)
	defer cancel()
	m = &Meta{}
	if m.Teams, err = cl.Teams(ctx); err != nil {
		return nil, err
	}
	if m.Schedules, err = cl.Schedules(ctx); err != nil {
		return nil, err
	}
	if m.Policies, err = cl.EscalationPolicies(ctx); err != nil {
		return nil, err
	}
	if m.Services, err = cl.Services(ctx); err != nil {
		return nil, err
	}
	if m.Users, err = cl.Users(ctx); err != nil {
		return nil, err
	}
	s.mu.Lock()
	s.metas[id] = m
	s.mu.Unlock()
	return m, nil
}

// people applies the filters and returns the matching people.
func (s *Server) people(r *http.Request) (any, int, error) {
	var in struct {
		Conn        string      `json:"conn"`
		From, To    string      `json:"-"`
		FromD       string      `json:"from"`
		ToD         string      `json:"to"`
		ReportTZ    string      `json:"reportTZ"`
		TeamIDs     []string    `json:"teamIds"`
		ScheduleIDs []string    `json:"scheduleIds"`
		PolicyIDs   []string    `json:"policyIds"`
		ServiceIDs  []string    `json:"serviceIds"`
		Roles       []string    `json:"roles"`
		UserIDs     []string    `json:"userIds"` // explicit list (CSV import)
		Settings    pd.Settings `json:"settings"`
	}
	if err := decode(r, &in); err != nil {
		return nil, 0, err
	}
	m, err := s.getMeta(r.Context(), in.Conn, false)
	if err != nil {
		return nil, 0, err
	}
	policyIDs := append([]string{}, in.PolicyIDs...)
	if len(in.ServiceIDs) > 0 {
		want := toSet(in.ServiceIDs)
		for _, sv := range m.Services {
			if want[sv.ID] {
				policyIDs = append(policyIDs, sv.EscalationPolicy.ID)
			}
		}
		if len(policyIDs) == 0 {
			return []pd.User{}, 0, nil
		}
	}
	var allowed map[string]bool
	if len(in.ScheduleIDs) > 0 || len(policyIDs) > 0 {
		cl, err := s.client(in.Conn, in.Settings)
		if err != nil {
			return nil, 0, err
		}
		loc, err := time.LoadLocation(in.ReportTZ)
		if err != nil {
			loc = time.UTC
		}
		f, e1 := time.ParseInLocation("2006-01-02", in.FromD, loc)
		t, e2 := time.ParseInLocation("2006-01-02", in.ToD, loc)
		if e1 != nil || e2 != nil {
			return nil, 0, fmt.Errorf("pick the dates first so the schedule and policy filters can be applied")
		}
		ocs, fails := cl.OnCalls(r.Context(), f, t.AddDate(0, 0, 1), pd.OnCallFilters{ScheduleIDs: in.ScheduleIDs, PolicyIDs: policyIDs})
		if len(fails) > 0 {
			return nil, 0, fmt.Errorf("could not read on-call data: %s", fails[0].Err)
		}
		allowed = map[string]bool{}
		for _, o := range ocs {
			allowed[o.UserID] = true
		}
	}
	teams, roles, ids := toSet(in.TeamIDs), toSet(in.Roles), toSet(in.UserIDs)
	out := []map[string]any{}
	for _, u := range m.Users {
		if len(teams) > 0 && !hasTeam(u, teams) {
			continue
		}
		if len(roles) > 0 && !roles[u.Role] {
			continue
		}
		if len(ids) > 0 && !ids[u.ID] {
			continue
		}
		if allowed != nil && !allowed[u.ID] {
			continue
		}
		tn := []string{}
		for _, t := range u.Teams {
			tn = append(tn, t.Name)
		}
		out = append(out, map[string]any{"id": u.ID, "name": u.Name, "email": u.Email, "timeZone": u.TimeZone,
			"role": u.Role, "team": strings.Join(tn, ", "), "country": report.CountryForTZ(u.TimeZone)})
	}
	return out, 0, nil
}

func toSet(s []string) map[string]bool {
	m := map[string]bool{}
	for _, v := range s {
		m[v] = true
	}
	return m
}

func hasTeam(u pd.User, want map[string]bool) bool {
	for _, t := range u.Teams {
		if want[t.ID] {
			return true
		}
	}
	return false
}

func (s *Server) saveReport(r *http.Request) (any, int, error) {
	var in store.SavedReport
	if err := decode(r, &in); err != nil {
		return nil, 0, err
	}
	out, err := s.Store.SaveReport(in)
	if err != nil {
		return nil, 0, err
	}
	return out, 0, nil
}

func (s *Server) delReport(r *http.Request) (any, int, error) {
	if err := s.Store.DeleteReport(r.PathValue("id")); err != nil {
		return nil, 500, err
	}
	return map[string]bool{"ok": true}, 0, nil
}

func (s *Server) holidays(r *http.Request) (any, int, error) {
	y, _ := strconv.Atoi(r.URL.Query().Get("year"))
	if y == 0 {
		y = time.Now().Year()
	}
	d, err := s.Hol.Dates(r.Context(), r.URL.Query().Get("country"), y)
	if err != nil {
		return nil, 0, err
	}
	return map[string]any{"dates": d}, 0, nil
}
