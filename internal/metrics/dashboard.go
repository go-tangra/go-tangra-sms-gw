package metrics

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// The monitoring dashboard runs only the module's predefined queries (the
// legacy dashboard's PromQL over the series above) with window presets;
// callers name queries, never supply PromQL. Ranges, steps, response sizes
// and timeouts are bounded.

// Window is a dashboard time window preset (legacy WINDOWS).
type Window struct {
	Key          string
	Range        time.Duration
	Step         time.Duration
	RateBracket  string // rate(...[x]) of the time series
	Lookback     string // increase(...[x]) of tables and the p95 quantiles
	HeadlineRate string // headline rates
}

// Windows are the permitted presets, 15m through 7d.
var Windows = map[string]Window{
	"15m": {"15m", 15 * time.Minute, 15 * time.Second, "1m", "15m", "1m"},
	"1h":  {"1h", time.Hour, time.Minute, "1m", "1h", "5m"},
	"6h":  {"6h", 6 * time.Hour, 6 * time.Minute, "5m", "6h", "5m"},
	"24h": {"24h", 24 * time.Hour, 24 * time.Minute, "5m", "24h", "5m"},
	"7d":  {"7d", 7 * 24 * time.Hour, 168 * time.Minute, "15m", "7d", "15m"},
}

// InstantQueries are the predefined instant queries by name.
var InstantQueries = map[string]func(Window) string{
	"sends_per_sec": func(w Window) string { return "sum(rate(sms_gw_send_total[" + w.HeadlineRate + "]))" },
	"success_rate": func(w Window) string {
		return `sum(rate(sms_gw_send_total{outcome="ok"}[` + w.HeadlineRate + "])) / clamp_min(sum(rate(sms_gw_send_total[" + w.HeadlineRate + "])), 1e-9)"
	},
	"send_p95_seconds": func(w Window) string {
		return "histogram_quantile(0.95, sum by (le) (rate(sms_gw_send_duration_seconds_bucket[" + w.Lookback + "])))"
	},
	"dlrs_per_sec": func(w Window) string { return "sum(rate(sms_gw_dlr_received_total[" + w.HeadlineRate + "]))" },
	"delivery_p95_seconds": func(w Window) string {
		return "histogram_quantile(0.95, sum by (le) (rate(sms_gw_delivery_latency_seconds_bucket[" + w.Lookback + "])))"
	},
	"send_by_outcome": func(w Window) string { return "sum by (outcome) (increase(sms_gw_send_total[" + w.Lookback + "]))" },
	"dlr_by_status": func(w Window) string {
		return "sum by (status) (increase(sms_gw_dlr_received_total[" + w.Lookback + "]))"
	},
	"webhook_by_outcome": func(w Window) string {
		return "sum by (outcome) (increase(sms_gw_webhook_delivery_total[" + w.Lookback + "]))"
	},
	"login_by_outcome": func(w Window) string {
		return "sum by (outcome) (increase(sms_gw_auth_login_total[" + w.Lookback + "]))"
	},
	"top_providers": func(w Window) string {
		return "sum by (provider, outcome) (increase(sms_gw_send_total[" + w.Lookback + "]))"
	},
}

// RangeQueries are the predefined range queries by name.
var RangeQueries = map[string]func(Window) string{
	"send_rate_by_outcome": func(w Window) string { return "sum by (outcome) (rate(sms_gw_send_total[" + w.RateBracket + "]))" },
	"send_latency_p95": func(w Window) string {
		return "histogram_quantile(0.95, sum by (le) (rate(sms_gw_send_duration_seconds_bucket[" + w.RateBracket + "])))"
	},
	"dlr_rate_by_status": func(w Window) string {
		return "sum by (status) (rate(sms_gw_dlr_received_total[" + w.RateBracket + "]))"
	},
}

// Bounds of one dashboard request.
const (
	maxSeries        = 200
	maxResponseBytes = 4 << 20
	queryTimeout     = 10 * time.Second
	maxConcurrent    = 4
)

// Dashboard errors.
var (
	ErrUnknownWindow = errors.New("metrics: unknown window")
	ErrUnknownQuery  = errors.New("metrics: unknown query")
)

// Unavailable reasons.
const (
	NotConfigured = "not_configured"
	Unreachable   = "unreachable"
)

// Sample is one instant value; NaN and ±Inf have no value.
type Sample struct {
	Labels   map[string]string `json:"labels"`
	Value    float64           `json:"value"`
	HasValue bool              `json:"has_value"`
}

// Series is one range series; a nil value is NaN or ±Inf.
type Series struct {
	Labels     map[string]string `json:"labels"`
	Timestamps []int64           `json:"timestamps"`
	Values     []*float64        `json:"values"`
}

// InstantResult answers an instant dashboard request.
type InstantResult struct {
	Available bool                `json:"available"`
	Reason    string              `json:"reason,omitempty"`
	Window    string              `json:"window"`
	Results   map[string][]Sample `json:"results"`
}

// RangeResult answers a range dashboard request.
type RangeResult struct {
	Available   bool                `json:"available"`
	Reason      string              `json:"reason,omitempty"`
	Window      string              `json:"window"`
	Start       int64               `json:"start,omitempty"`
	End         int64               `json:"end,omitempty"`
	StepSeconds int64               `json:"step_seconds,omitempty"`
	Results     map[string][]Series `json:"results"`
}

// Dashboard queries Prometheus; a nil *Dashboard is "not configured".
type Dashboard struct {
	base   *url.URL
	client *http.Client
	now    func() time.Time
}

// NewDashboard returns nil when prometheusURL is empty (monitoring off).
func NewDashboard(prometheusURL string) (*Dashboard, error) {
	if prometheusURL == "" {
		return nil, nil
	}
	u, err := url.Parse(strings.TrimSuffix(prometheusURL, "/"))
	if err != nil || (u.Scheme != "http" && u.Scheme != "https") || u.Host == "" || u.User != nil {
		return nil, errors.New("metrics: monitoring URL must be an http(s) URL without credentials")
	}
	return &Dashboard{base: u, now: time.Now, client: &http.Client{Timeout: queryTimeout,
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}}, nil
}

func selectQueries(all map[string]func(Window) string, names []string) ([]string, error) {
	if len(names) == 0 {
		for n := range all {
			names = append(names, n)
		}
		sort.Strings(names)
		return names, nil
	}
	for _, n := range names {
		if all[n] == nil {
			return nil, ErrUnknownQuery
		}
	}
	return names, nil
}

// fanOut runs fn for every name with bounded concurrency; the first error wins.
func fanOut(names []string, fn func(string) error) error {
	var (
		wg    sync.WaitGroup
		mu    sync.Mutex
		first error
	)
	sem := make(chan struct{}, maxConcurrent)
	for _, n := range names {
		wg.Add(1)
		sem <- struct{}{}
		go func() {
			defer func() { <-sem; wg.Done() }()
			if err := fn(n); err != nil {
				mu.Lock()
				if first == nil {
					first = err
				}
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	return first
}

// Instant evaluates the named instant queries (all when names is empty) at
// the current time. Monitoring that is off or failing is reported in the
// result, not as an error; unknown windows and queries are errors.
func (d *Dashboard) Instant(ctx context.Context, window string, names []string) (InstantResult, error) {
	w, ok := Windows[window]
	if !ok {
		return InstantResult{}, ErrUnknownWindow
	}
	names, err := selectQueries(InstantQueries, names)
	if err != nil {
		return InstantResult{}, err
	}
	res := InstantResult{Window: w.Key, Results: map[string][]Sample{}}
	if d == nil {
		res.Reason = NotConfigured
		return res, nil
	}
	var mu sync.Mutex
	err = fanOut(names, func(n string) error {
		s, err := d.instant(ctx, InstantQueries[n](w))
		mu.Lock()
		res.Results[n] = s
		mu.Unlock()
		return err
	})
	if err != nil {
		return InstantResult{Window: w.Key, Reason: Unreachable, Results: map[string][]Sample{}}, nil
	}
	res.Available = true
	return res, nil
}

// Range evaluates the named range queries over the window ending now.
func (d *Dashboard) Range(ctx context.Context, window string, names []string) (RangeResult, error) {
	w, ok := Windows[window]
	if !ok {
		return RangeResult{}, ErrUnknownWindow
	}
	names, err := selectQueries(RangeQueries, names)
	if err != nil {
		return RangeResult{}, err
	}
	res := RangeResult{Window: w.Key, Results: map[string][]Series{}}
	if d == nil {
		res.Reason = NotConfigured
		return res, nil
	}
	end := d.now().Truncate(time.Second)
	start := end.Add(-w.Range)
	var mu sync.Mutex
	err = fanOut(names, func(n string) error {
		s, err := d.rangeQuery(ctx, RangeQueries[n](w), start, end, w.Step)
		mu.Lock()
		res.Results[n] = s
		mu.Unlock()
		return err
	})
	if err != nil {
		return RangeResult{Window: w.Key, Reason: Unreachable, Results: map[string][]Series{}}, nil
	}
	res.Available, res.Start, res.End, res.StepSeconds = true, start.Unix(), end.Unix(), int64(w.Step/time.Second)
	return res, nil
}

type promResponse struct {
	Status string `json:"status"`
	Data   struct {
		ResultType string            `json:"resultType"`
		Result     []json.RawMessage `json:"result"`
	} `json:"data"`
}

func (d *Dashboard) get(ctx context.Context, path string, q url.Values) (promResponse, error) {
	var out promResponse
	ctx, cancel := context.WithTimeout(ctx, queryTimeout)
	defer cancel()
	u := *d.base
	u.Path += path
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return out, err
	}
	resp, err := d.client.Do(req)
	if err != nil {
		return out, errors.New("metrics: monitoring unreachable")
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	if err != nil || len(body) > maxResponseBytes {
		return out, errors.New("metrics: monitoring response unusable")
	}
	if resp.StatusCode/100 != 2 || json.Unmarshal(body, &out) != nil || out.Status != "success" {
		return out, fmt.Errorf("metrics: monitoring answered %d", resp.StatusCode)
	}
	if len(out.Data.Result) > maxSeries {
		out.Data.Result = out.Data.Result[:maxSeries]
	}
	return out, nil
}

func (d *Dashboard) instant(ctx context.Context, query string) ([]Sample, error) {
	r, err := d.get(ctx, "/api/v1/query", url.Values{"query": {query}, "time": {seconds(d.now())}})
	if err != nil {
		return nil, err
	}
	out := []Sample{}
	switch r.Data.ResultType {
	case "vector":
		for _, raw := range r.Data.Result {
			var item struct {
				Metric map[string]string `json:"metric"`
				Value  [2]any            `json:"value"`
			}
			if json.Unmarshal(raw, &item) != nil {
				return nil, errors.New("metrics: malformed vector")
			}
			v, ok := value(item.Value)
			out = append(out, Sample{Labels: labels(item.Metric), Value: v, HasValue: ok})
		}
	case "scalar":
		if len(r.Data.Result) == 2 {
			var pair [2]any
			_ = json.Unmarshal(r.Data.Result[0], &pair[0])
			_ = json.Unmarshal(r.Data.Result[1], &pair[1])
			v, ok := value(pair)
			out = append(out, Sample{Labels: map[string]string{}, Value: v, HasValue: ok})
		}
	default:
		return nil, errors.New("metrics: unexpected result type")
	}
	return out, nil
}

func (d *Dashboard) rangeQuery(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error) {
	r, err := d.get(ctx, "/api/v1/query_range", url.Values{"query": {query}, "start": {seconds(start)}, "end": {seconds(end)},
		"step": {strconv.FormatInt(int64(step/time.Second), 10)}})
	if err != nil {
		return nil, err
	}
	if r.Data.ResultType != "matrix" {
		return nil, errors.New("metrics: unexpected result type")
	}
	maxPoints := int(end.Sub(start)/step) + 2
	out := []Series{}
	for _, raw := range r.Data.Result {
		var item struct {
			Metric map[string]string `json:"metric"`
			Values [][2]any          `json:"values"`
		}
		if json.Unmarshal(raw, &item) != nil {
			return nil, errors.New("metrics: malformed matrix")
		}
		if len(item.Values) > maxPoints {
			item.Values = item.Values[:maxPoints]
		}
		s := Series{Labels: labels(item.Metric), Timestamps: make([]int64, 0, len(item.Values)), Values: make([]*float64, 0, len(item.Values))}
		for _, p := range item.Values {
			ts, _ := p[0].(float64)
			s.Timestamps = append(s.Timestamps, int64(ts))
			if v, ok := value(p); ok {
				s.Values = append(s.Values, &v)
			} else {
				s.Values = append(s.Values, nil)
			}
		}
		out = append(out, s)
	}
	return out, nil
}

func seconds(t time.Time) string { return strconv.FormatInt(t.Unix(), 10) }

func labels(m map[string]string) map[string]string {
	if m == nil {
		return map[string]string{}
	}
	return m
}

// value decodes a Prometheus [time, "value"] pair; NaN and ±Inf have none.
func value(p [2]any) (float64, bool) {
	s, ok := p[1].(string)
	if !ok {
		return 0, false
	}
	v, err := strconv.ParseFloat(s, 64)
	if err != nil || math.IsNaN(v) || math.IsInf(v, 0) {
		return 0, false
	}
	return v, true
}
