package pd

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"
)

func testClient(url string) (*Client, *[]time.Duration) {
	c := New("tok", "us", Settings{DelayMS: 0, MaxRetries: 3, TimeoutS: 5})
	c.BaseURL = url
	var slept []time.Duration
	c.sleep = func(_ context.Context, d time.Duration) error { slept = append(slept, d); return nil }
	return c, &slept
}

// The server returns fewer rows than the requested limit but says more=true: we must keep going.
func TestPaginationShortPages(t *testing.T) {
	total := 7
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		off, _ := strconv.Atoi(r.URL.Query().Get("offset"))
		n := 3 // always return only 3 rows
		end := min(off+n, total)
		rows := []map[string]string{}
		for i := off; i < end; i++ {
			rows = append(rows, map[string]string{"id": fmt.Sprint(i), "name": fmt.Sprint("t", i)})
		}
		json.NewEncoder(w).Encode(map[string]any{"teams": rows, "more": end < total, "limit": 100, "offset": off})
	}))
	defer srv.Close()
	c, _ := testClient(srv.URL)
	got, err := c.Teams(context.Background())
	if err != nil || len(got) != total {
		t.Fatalf("got %d teams, err %v", len(got), err)
	}
}

func TestRetries429And5xx(t *testing.T) {
	var n int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch atomic.AddInt32(&n, 1) {
		case 1:
			w.Header().Set("ratelimit-reset", "7")
			w.WriteHeader(429)
		case 2:
			w.WriteHeader(503)
		default:
			w.Write([]byte(`{"abilities":["analytics"]}`))
		}
	}))
	defer srv.Close()
	c, slept := testClient(srv.URL)
	ab, err := c.Validate(context.Background())
	if err != nil || ab != "analytics" {
		t.Fatalf("%q %v", ab, err)
	}
	if c.Retries != 2 || c.Waits != 1 {
		t.Fatalf("retries=%d waits=%d", c.Retries, c.Waits)
	}
	if (*slept)[0] < 8*time.Second { // ratelimit-reset 7 + 1s buffer must be honored
		t.Fatalf("did not honor reset header: %v", *slept)
	}
}

func TestGivesUpAndReports(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(500) }))
	defer srv.Close()
	c, _ := testClient(srv.URL)
	if _, err := c.Teams(context.Background()); err == nil {
		t.Fatal("expected error after retries")
	}
	if c.Calls != 4 {
		t.Fatalf("calls=%d want 4", c.Calls)
	}
}

func TestNoRetryOn4xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(403) }))
	defer srv.Close()
	c, _ := testClient(srv.URL)
	_, err := c.Teams(context.Background())
	if !IsEntitlement(err) || c.Calls != 1 {
		t.Fatalf("err=%v calls=%d", err, c.Calls)
	}
}

func TestOnCallsClipDedupeNullEnd(t *testing.T) {
	from := time.Date(2026, 9, 1, 0, 0, 0, 0, time.UTC)
	to := from.AddDate(0, 0, 10)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		o := func(lvl int, s, e any) map[string]any {
			return map[string]any{"escalation_level": lvl, "start": s, "end": e, "user": map[string]string{"id": "U1", "name": "A"}, "escalation_policy": map[string]string{"id": "E1"}}
		}
		json.NewEncoder(w).Encode(map[string]any{"oncalls": []any{
			o(1, "2026-08-30T00:00:00Z", "2026-09-02T00:00:00Z"), // starts before range: clipped
			o(1, "2026-08-30T00:00:00Z", "2026-09-02T00:00:00Z"), // duplicate: dropped
			o(2, nil, nil), // null start/end: whole window
		}, "more": false})
	}))
	defer srv.Close()
	c, _ := testClient(srv.URL)
	got, fails := c.OnCalls(context.Background(), from, to, OnCallFilters{UserIDs: []string{"U1"}})
	if len(fails) != 0 || len(got) != 2 {
		t.Fatalf("%v %v", got, fails)
	}
	if !got[0].Start.Equal(from) || !got[1].End.Equal(to) {
		t.Fatalf("%v", got)
	}
}

// Analytics may return a single aggregated row without responder_id: engaged time must still be
// attributed to the right people (by querying them one at a time).
func TestEngagedAggregatedRowsAttributed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var body struct {
			Filters struct {
				IDs []string `json:"responder_ids"`
			} `json:"filters"`
		}
		json.NewDecoder(r.Body).Decode(&body)
		// one aggregated row, 100s per responder asked about, and no responder_id
		json.NewEncoder(w).Encode(map[string]any{"data": []any{map[string]any{"total_engaged_seconds": 100 * len(body.Filters.IDs)}}, "more": false})
	}))
	defer srv.Close()
	c, _ := testClient(srv.URL)
	got, err := c.EngagedSeconds(context.Background(), time.Now(), time.Now().Add(time.Hour), []string{"A", "B", "C"})
	if err != nil || got["A"] != 100 || got["B"] != 100 || got["C"] != 100 {
		t.Fatalf("%v %v", got, err)
	}
	if got[""] != 0 {
		t.Fatal("seconds stored under an empty id")
	}
}
