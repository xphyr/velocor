package monitoring

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"time"
)

// Alert is an alert as the UI shows it (from Alertmanager's API).
type Alert struct {
	Name        string            `json:"name"`
	Severity    string            `json:"severity"`
	Summary     string            `json:"summary"`
	Description string            `json:"description"`
	Labels      map[string]string `json:"labels"`
	StartsAt    time.Time         `json:"starts_at"`
	// State is "active" or "suppressed" (silenced or inhibited).
	State      string   `json:"state"`
	SilencedBy []string `json:"silenced_by,omitempty"`
}

// Matcher selects alerts by a label's exact value.
type Matcher struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// Silence mutes the alerts its matchers select until EndsAt.
type Silence struct {
	ID        string    `json:"id"`
	Matchers  []Matcher `json:"matchers"`
	StartsAt  time.Time `json:"starts_at"`
	EndsAt    time.Time `json:"ends_at"`
	CreatedBy string    `json:"created_by"`
	Comment   string    `json:"comment"`
	State     string    `json:"state"` // active, pending, expired
}

func (m *Manager) do(ctx context.Context, method, base, path string, body any, out any) error {
	var rd io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, "http://"+base+path, rd)
	if err != nil {
		return err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := m.client().Do(req)
	if err != nil {
		return fmt.Errorf("%w: %v", ErrUnavailable, err)
	}
	defer func() { _ = resp.Body.Close() }()
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 32<<20))
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("%s %s: %s: %s", method, path, resp.Status, bytes.TrimSpace(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// ErrUnavailable means the built-in service doesn't answer (off, or still starting).
var ErrUnavailable = fmt.Errorf("built-in monitoring isn't running")

// Alerts lists firing alerts, the watchdog left out, most severe and oldest first.
func (m *Manager) Alerts(ctx context.Context) ([]Alert, error) {
	var raw []struct {
		Labels      map[string]string `json:"labels"`
		Annotations map[string]string `json:"annotations"`
		StartsAt    time.Time         `json:"startsAt"`
		Status      struct {
			State      string   `json:"state"`
			SilencedBy []string `json:"silencedBy"`
		} `json:"status"`
	}
	if err := m.do(ctx, http.MethodGet, m.amAddr(), "/api/v2/alerts", nil, &raw); err != nil {
		return nil, err
	}
	out := []Alert{}
	for _, a := range raw {
		if a.Labels["alertname"] == "VelocorWatchdog" {
			continue
		}
		labels := map[string]string{}
		for k, v := range a.Labels {
			if k != "alertname" && k != "severity" && k != "job" {
				labels[k] = v
			}
		}
		out = append(out, Alert{Name: a.Labels["alertname"], Severity: a.Labels["severity"], Summary: a.Annotations["summary"],
			Description: a.Annotations["description"], Labels: labels, StartsAt: a.StartsAt, State: a.Status.State, SilencedBy: a.Status.SilencedBy})
	}
	rank := map[string]int{"critical": 0, "warning": 1}
	sort.SliceStable(out, func(i, j int) bool {
		ri, okI := rank[out[i].Severity]
		rj, okJ := rank[out[j].Severity]
		if !okI {
			ri = 2
		}
		if !okJ {
			rj = 2
		}
		if ri != rj {
			return ri < rj
		}
		return out[i].StartsAt.Before(out[j].StartsAt)
	})
	return out, nil
}

// Silences lists silences that are active or pending.
func (m *Manager) Silences(ctx context.Context) ([]Silence, error) {
	var raw []struct {
		ID       string `json:"id"`
		Matchers []struct {
			Name    string `json:"name"`
			Value   string `json:"value"`
			IsRegex bool   `json:"isRegex"`
		} `json:"matchers"`
		StartsAt  time.Time `json:"startsAt"`
		EndsAt    time.Time `json:"endsAt"`
		CreatedBy string    `json:"createdBy"`
		Comment   string    `json:"comment"`
		Status    struct {
			State string `json:"state"`
		} `json:"status"`
	}
	if err := m.do(ctx, http.MethodGet, m.amAddr(), "/api/v2/silences", nil, &raw); err != nil {
		return nil, err
	}
	out := []Silence{}
	for _, s := range raw {
		if s.Status.State == "expired" {
			continue
		}
		sl := Silence{ID: s.ID, StartsAt: s.StartsAt, EndsAt: s.EndsAt, CreatedBy: s.CreatedBy, Comment: s.Comment, State: s.Status.State}
		for _, mt := range s.Matchers {
			sl.Matchers = append(sl.Matchers, Matcher{Name: mt.Name, Value: mt.Value})
		}
		out = append(out, sl)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].EndsAt.Before(out[j].EndsAt) })
	return out, nil
}

// CreateSilence mutes alerts matching every matcher (exact values) for d.
func (m *Manager) CreateSilence(ctx context.Context, matchers []Matcher, d time.Duration, createdBy, comment string) (string, error) {
	if len(matchers) == 0 {
		return "", fmt.Errorf("a silence needs at least one matcher")
	}
	type amMatcher struct {
		Name    string `json:"name"`
		Value   string `json:"value"`
		IsRegex bool   `json:"isRegex"`
		IsEqual bool   `json:"isEqual"`
	}
	ms := make([]amMatcher, len(matchers))
	for i, mt := range matchers {
		ms[i] = amMatcher{Name: mt.Name, Value: mt.Value, IsEqual: true}
	}
	now := time.Now().UTC()
	body := map[string]any{"matchers": ms, "startsAt": now, "endsAt": now.Add(d), "createdBy": createdBy, "comment": comment}
	var res struct {
		SilenceID string `json:"silenceID"`
	}
	if err := m.do(ctx, http.MethodPost, m.amAddr(), "/api/v2/silences", body, &res); err != nil {
		return "", err
	}
	return res.SilenceID, nil
}

// ExpireSilence ends a silence now.
func (m *Manager) ExpireSilence(ctx context.Context, id string) error {
	return m.do(ctx, http.MethodDelete, m.amAddr(), "/api/v2/silence/"+url.PathEscape(id), nil, nil)
}

// Point is one sample of a series.
type Point struct {
	T int64   `json:"t"` // unix seconds
	V float64 `json:"v"`
}

// Series is one line of a graph.
type Series struct {
	Labels map[string]string `json:"labels"`
	Points []Point           `json:"points"`
}

// QueryRange runs a PromQL range query on the built-in Prometheus.
func (m *Manager) QueryRange(ctx context.Context, query string, start, end time.Time, step time.Duration) ([]Series, error) {
	q := url.Values{}
	q.Set("query", query)
	q.Set("start", strconv.FormatInt(start.Unix(), 10))
	q.Set("end", strconv.FormatInt(end.Unix(), 10))
	q.Set("step", strconv.FormatFloat(step.Seconds(), 'f', -1, 64))
	var res struct {
		Status string `json:"status"`
		Data   struct {
			Result []struct {
				Metric map[string]string `json:"metric"`
				Values [][2]any          `json:"values"`
			} `json:"result"`
		} `json:"data"`
	}
	if err := m.do(ctx, http.MethodGet, m.promAddr(), "/api/v1/query_range?"+q.Encode(), nil, &res); err != nil {
		return nil, err
	}
	out := make([]Series, 0, len(res.Data.Result))
	for _, r := range res.Data.Result {
		s := Series{Labels: r.Metric, Points: make([]Point, 0, len(r.Values))}
		for _, v := range r.Values {
			ts, _ := v[0].(float64)
			str, _ := v[1].(string)
			f, err := strconv.ParseFloat(str, 64)
			if err != nil {
				continue
			}
			s.Points = append(s.Points, Point{T: int64(ts), V: f})
		}
		out = append(out, s)
	}
	return out, nil
}

// SendTestAlert posts a short-lived warning straight to Alertmanager, so an administrator can
// check that receivers deliver.
func (m *Manager) SendTestAlert(ctx context.Context, by string) error {
	now := time.Now().UTC()
	alert := []map[string]any{{
		"labels": map[string]string{"alertname": "VelocorTestAlert", "severity": "warning", "instance": m.Instance},
		"annotations": map[string]string{
			"summary":     "Test alert from Velocor",
			"description": "Sent by " + by + " from the appliance's alerting settings, to check that alerts are delivered. It resolves by itself.",
		},
		"startsAt": now, "endsAt": now.Add(5 * time.Minute),
	}}
	return m.do(ctx, http.MethodPost, m.amAddr(), "/api/v2/alerts", alert, nil)
}
