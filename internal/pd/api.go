package pd

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"oncallcalc/internal/engine"
)

// Ref is a named PagerDuty object.
type Ref struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

// User is a PagerDuty user as shown in the people list.
type User struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Email    string `json:"email"`
	TimeZone string `json:"time_zone"`
	Role     string `json:"role"`
	Teams    []Ref  `json:"teams"`
}

// Validate checks the token and returns the account subdomain if available.
func (c *Client) Validate(ctx context.Context) (string, error) {
	data, err := c.Do(ctx, "GET", "/abilities", nil, nil)
	if err != nil {
		return "", err
	}
	var r struct {
		Abilities []string `json:"abilities"`
	}
	_ = json.Unmarshal(data, &r)
	return strings.Join(r.Abilities, ","), nil
}

// HasAnalytics reports whether the account lists the analytics ability.
func HasAnalytics(abilities string) bool {
	return strings.Contains(abilities, "analytics") || strings.Contains(abilities, "responder_insights")
}

func (c *Client) listRefs(ctx context.Context, path, key string, q map[string][]string) ([]Ref, error) {
	var out []Ref
	err := c.Paginate(ctx, path, q, func(page json.RawMessage) error {
		var env map[string]json.RawMessage
		if err := json.Unmarshal(page, &env); err != nil {
			return err
		}
		var rows []Ref
		if err := json.Unmarshal(env[key], &rows); err != nil {
			return fmt.Errorf("unexpected %s response: %w", key, err)
		}
		out = append(out, rows...)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, err
}

// Teams, Schedules, EscalationPolicies, Services list the filter choices.
func (c *Client) Teams(ctx context.Context) ([]Ref, error) {
	return c.listRefs(ctx, "/teams", "teams", nil)
}
func (c *Client) Schedules(ctx context.Context) ([]Ref, error) {
	return c.listRefs(ctx, "/schedules", "schedules", nil)
}
func (c *Client) EscalationPolicies(ctx context.Context) ([]Ref, error) {
	return c.listRefs(ctx, "/escalation_policies", "escalation_policies", nil)
}

// Service maps a service to its escalation policy.
type Service struct {
	Ref
	EscalationPolicy Ref `json:"escalation_policy"`
}

func (c *Client) Services(ctx context.Context) ([]Service, error) {
	var out []Service
	err := c.Paginate(ctx, "/services", nil, func(page json.RawMessage) error {
		var env struct {
			Services []Service `json:"services"`
		}
		if err := json.Unmarshal(page, &env); err != nil {
			return err
		}
		out = append(out, env.Services...)
		return nil
	})
	return out, err
}

// Users lists every user with team memberships and time zone (from the Get User fields).
func (c *Client) Users(ctx context.Context) ([]User, error) {
	var out []User
	err := c.Paginate(ctx, "/users", map[string][]string{"include[]": {"teams"}}, func(page json.RawMessage) error {
		var env struct {
			Users []User `json:"users"`
		}
		if err := json.Unmarshal(page, &env); err != nil {
			return err
		}
		out = append(out, env.Users...)
		return nil
	})
	sort.Slice(out, func(i, j int) bool { return strings.ToLower(out[i].Name) < strings.ToLower(out[j].Name) })
	return out, err
}

// OnCall is one on-call period.
type OnCall struct {
	UserID string
	Level  int
	engine.Interval
}

// OnCallFilters narrows the on-call query.
type OnCallFilters struct {
	UserIDs, ScheduleIDs, PolicyIDs []string
}

// Failure records a fetch that still failed after all retries.
type Failure struct {
	UserIDs  []string  `json:"userIds"`
	From, To time.Time `json:"-"`
	Err      string    `json:"error"`
}

// OnCalls returns on-call periods overlapping [from, to), clipped to that range. Requests are
// split into windows of at most 30 days and into batches of users so each stays well within the
// API's pagination limits; every page is followed to the end.
func (c *Client) OnCalls(ctx context.Context, from, to time.Time, f OnCallFilters) ([]OnCall, []Failure) {
	var out []OnCall
	var fails []Failure
	seen := map[string]bool{}
	batches := chunk(f.UserIDs, 25)
	if len(batches) == 0 {
		batches = [][]string{nil}
	}
	for ws := from; ws.Before(to); ws = ws.AddDate(0, 0, 30) {
		we := ws.AddDate(0, 0, 30)
		if we.After(to) {
			we = to
		}
		for _, b := range batches {
			q := map[string][]string{
				"since":      {ws.UTC().Format(time.RFC3339)},
				"until":      {we.UTC().Format(time.RFC3339)},
				"earliest":   {"false"},
				"time_zone":  {"UTC"},
				"user_ids[]": b,
			}
			if len(f.ScheduleIDs) > 0 {
				q["schedule_ids[]"] = f.ScheduleIDs
			}
			if len(f.PolicyIDs) > 0 {
				q["escalation_policy_ids[]"] = f.PolicyIDs
			}
			err := c.Paginate(ctx, "/oncalls", q, func(page json.RawMessage) error {
				var env struct {
					OnCalls []struct {
						Level int     `json:"escalation_level"`
						Start *string `json:"start"`
						End   *string `json:"end"`
						User  Ref     `json:"user"`
						Sched *Ref    `json:"schedule"`
						EP    Ref     `json:"escalation_policy"`
					} `json:"oncalls"`
				}
				if err := json.Unmarshal(page, &env); err != nil {
					return err
				}
				for _, o := range env.OnCalls {
					s, e := ws, we // a null start/end means the on-call covers the whole requested window
					if o.Start != nil {
						if t, err := time.Parse(time.RFC3339, *o.Start); err == nil {
							s = t
						}
					}
					if o.End != nil {
						if t, err := time.Parse(time.RFC3339, *o.End); err == nil {
							e = t
						}
					}
					if s.Before(ws) {
						s = ws
					}
					if e.After(we) {
						e = we
					}
					if !s.Before(e) {
						continue
					}
					// The same period can be listed once per policy/schedule; keep one copy per user/level/span.
					key := fmt.Sprintf("%s|%d|%d|%d", o.User.ID, o.Level, s.Unix(), e.Unix())
					if seen[key] {
						continue
					}
					seen[key] = true
					out = append(out, OnCall{UserID: o.User.ID, Level: o.Level, Interval: engine.Interval{Start: s, End: e}})
				}
				return nil
			})
			if err != nil {
				if ctx.Err() != nil {
					return out, append(fails, Failure{UserIDs: b, From: ws, To: we, Err: ctx.Err().Error()})
				}
				fails = append(fails, Failure{UserIDs: b, From: ws, To: we, Err: err.Error()})
			}
		}
	}
	return out, fails
}

func chunk(s []string, n int) [][]string {
	var out [][]string
	for len(s) > 0 {
		k := min(n, len(s))
		out = append(out, s[:k])
		s = s[k:]
	}
	return out
}

// EngagedSeconds returns engaged seconds per responder for incidents in [from, to) using
// Analytics. All pages are followed. Some Analytics endpoints return one aggregated row for all
// the responders asked about, with no responder_id. When that happens the responders are queried
// one at a time so every number is attributed to the right person.
func (c *Client) EngagedSeconds(ctx context.Context, from, to time.Time, responderIDs []string) (map[string]float64, error) {
	out := map[string]float64{}
	for _, ids := range chunk(responderIDs, 100) {
		res, aggregated, err := c.engagedQuery(ctx, from, to, ids)
		if err != nil {
			return nil, err
		}
		if aggregated != nil && len(ids) == 1 {
			res = map[string]float64{ids[0]: *aggregated}
		} else if aggregated != nil {
			for _, id := range ids { // one by one: exact attribution
				r, agg, err := c.engagedQuery(ctx, from, to, []string{id})
				if err != nil {
					return nil, err
				}
				switch {
				case agg != nil:
					out[id] += *agg
				default:
					out[id] += r[id]
				}
			}
			continue
		}
		for id, v := range res {
			out[id] += v
		}
	}
	return out, nil
}

// engagedQuery runs one Analytics request. It returns per-responder seconds, or, when the
// response rows carry no responder_id, the aggregated total for everyone queried.
func (c *Client) engagedQuery(ctx context.Context, from, to time.Time, ids []string) (map[string]float64, *float64, error) {
	body := map[string]any{
		"filters": map[string]any{
			"date_range_start": from.UTC().Format(time.RFC3339),
			"date_range_end":   to.UTC().Format(time.RFC3339),
			"responder_ids":    ids,
		},
		"time_zone": "UTC",
	}
	res := map[string]float64{}
	var total float64
	missingID := false
	var starting any
	for page := 0; page < 200; page++ {
		if starting != nil {
			body["starting_after"] = starting
		}
		data, err := c.analyticsPost(ctx, body)
		if err != nil {
			return nil, nil, err
		}
		var env struct {
			Data []struct {
				ResponderID string  `json:"responder_id"`
				Engaged     float64 `json:"total_engaged_seconds"`
			} `json:"data"`
			More bool `json:"more"`
			Last any  `json:"last"`
		}
		if err := json.Unmarshal(data, &env); err != nil {
			return nil, nil, fmt.Errorf("unexpected analytics response: %w", err)
		}
		var sum float64
		for _, d := range env.Data {
			sum += d.Engaged
			if d.ResponderID == "" {
				missingID = true
				continue
			}
			res[d.ResponderID] += d.Engaged
		}
		total += sum
		c.emit("page", fmt.Sprintf("engaged %s..%s: %d rows, %.0fs engaged, per-responder ids: %v", from.UTC().Format("01-02 15:04"), to.UTC().Format("01-02 15:04"), len(env.Data), sum, !missingID))
		if !env.More || len(env.Data) == 0 || env.Last == nil {
			break
		}
		starting = env.Last
	}
	if missingID {
		return nil, &total, nil
	}
	return res, nil, nil
}

// analyticsPost tries the per-responder metrics path first and falls back to the "all" path if it
// does not exist. The path that works is remembered for the rest of the run.
func (c *Client) analyticsPost(ctx context.Context, body any) ([]byte, error) {
	c.mu.Lock()
	path := c.analyticsPath
	c.mu.Unlock()
	paths := []string{"/analytics/metrics/responders/", "/analytics/metrics/responders/all"}
	if path != "" {
		paths = []string{path}
	}
	var lastErr error
	for _, p := range paths {
		data, err := c.Do(ctx, "POST", p, nil, body)
		if err == nil {
			c.mu.Lock()
			c.analyticsPath = p
			c.mu.Unlock()
			return data, nil
		}
		var ae *APIError
		if !asAPIError(err, &ae) || ae.Status != 404 {
			return nil, err
		}
		lastErr = err
	}
	return nil, lastErr
}
