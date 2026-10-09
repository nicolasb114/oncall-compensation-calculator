package server

import (
	"context"
	"fmt"
	"net/http"
	"sync"
	"time"

	"oncallcalc/internal/export"
	"oncallcalc/internal/pd"
	"oncallcalc/internal/pricing"
	"oncallcalc/internal/report"
)

// Job is one running or finished report.
type Job struct {
	ID     string
	mu     sync.Mutex
	Status string // running, paused, done, error, cancelled
	Stage  string
	Done   int
	Total  int
	Log    []string
	Err    string
	Result *report.Result
	cancel context.CancelFunc
	cl     *pd.Client
	cfg    report.Config
	connID string
}

func (j *Job) logf(format string, a ...any) {
	j.mu.Lock()
	defer j.mu.Unlock()
	j.Log = append(j.Log, time.Now().Format("15:04:05 ")+fmt.Sprintf(format, a...))
	if len(j.Log) > 200 {
		j.Log = j.Log[len(j.Log)-200:]
	}
}

func (s *Server) run(r *http.Request) (any, int, error) {
	var in struct {
		Conn     string        `json:"conn"`
		Config   report.Config `json:"config"`
		Settings pd.Settings   `json:"settings"`
	}
	if err := decode(r, &in); err != nil {
		return nil, 0, err
	}
	cl, err := s.client(in.Conn, in.Settings)
	if err != nil {
		return nil, 0, err
	}
	if err := in.Config.Rules.Normalize(); err != nil {
		return nil, 0, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	j := &Job{ID: fmt.Sprintf("%d", time.Now().UnixNano()), Status: "running", Stage: "Starting", cancel: cancel, cl: cl, cfg: in.Config, connID: in.Conn}
	cl.OnEvent = func(e pd.Event) {
		switch e.Kind {
		case "retry":
			j.logf("%s", e.Msg)
		case "page":
			j.logf("%s", e.Msg)
		}
	}
	s.mu.Lock()
	s.jobs[j.ID] = j
	s.mu.Unlock()
	go func() {
		res, err := report.Run(ctx, cl, s.Hol, in.Config, func(stage string, done, total int) {
			j.mu.Lock()
			j.Stage, j.Done, j.Total = stage, done, total
			j.mu.Unlock()
		})
		j.mu.Lock()
		defer j.mu.Unlock()
		switch {
		case ctx.Err() != nil:
			j.Status = "cancelled"
		case err != nil:
			j.Status, j.Err = "error", friendly(err)
		default:
			j.Status, j.Result = "done", res
		}
	}()
	return map[string]string{"jobId": j.ID}, 0, nil
}

func (s *Server) job(id string) *Job {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.jobs[id]
}

func (s *Server) jobStatus(r *http.Request) (any, int, error) {
	j := s.job(r.PathValue("id"))
	if j == nil {
		return nil, 404, fmt.Errorf("unknown job")
	}
	j.mu.Lock()
	defer j.mu.Unlock()
	out := map[string]any{"includeEngaged": j.cfg.IncludeEngaged, "status": j.Status, "stage": j.Stage, "done": j.Done, "total": j.Total, "log": j.Log, "error": j.Err}
	if j.cl != nil {
		out["calls"], out["retries"], out["rateLimitWaits"] = j.cl.Calls, j.cl.Retries, j.cl.Waits
	}
	if j.Result != nil {
		out["result"] = j.Result
	}
	return out, 0, nil
}

func (s *Server) jobAction(r *http.Request) (any, int, error) {
	j := s.job(r.PathValue("id"))
	if j == nil {
		return nil, 404, fmt.Errorf("unknown job")
	}
	switch r.PathValue("action") {
	case "pause":
		j.cl.Pause()
		j.mu.Lock()
		if j.Status == "running" {
			j.Status = "paused"
		}
		j.mu.Unlock()
	case "resume":
		j.cl.Resume()
		j.mu.Lock()
		if j.Status == "paused" {
			j.Status = "running"
		}
		j.mu.Unlock()
	case "cancel":
		j.cancel()
		j.cl.Resume()
	case "reprice": // what-if: new prices on the data already fetched, no API calls
		var pc pricing.Config
		if err := decode(r, &pc); err != nil {
			return nil, 0, err
		}
		j.mu.Lock()
		defer j.mu.Unlock()
		if j.Result == nil {
			return nil, 0, fmt.Errorf("the report has not finished yet")
		}
		res := *j.Result
		res.Config.Pricing = pc
		res.People = append([]report.PersonResult(nil), j.Result.People...)
		for i := range res.People {
			if pc.Enabled {
				a := pc.Price(res.People[i].L1, res.People[i].L2, res.People[i].Engaged)
				res.People[i].Amounts = &a
			} else {
				res.People[i].Amounts = nil
			}
		}
		j.Result = &res
		return res, 0, nil
	default:
		return nil, 404, fmt.Errorf("unknown action")
	}
	return map[string]bool{"ok": true}, 0, nil
}

func (s *Server) exportJob(w http.ResponseWriter, r *http.Request) {
	j := s.job(r.PathValue("id"))
	if j == nil || j.Result == nil {
		http.Error(w, "report not ready", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	o := export.Options{Title: q.Get("title"), Detail: q.Get("detail") == "1", Rules: q.Get("rules") == "1", Anonymize: q.Get("anon") == "1"}
	j.mu.Lock()
	res := j.Result
	j.mu.Unlock()
	data, name, ctype, err := export.Render(res, q.Get("format"), o)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	w.Write(data)
}
