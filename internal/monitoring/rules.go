package monitoring

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	rules "github.com/velocor/velocor/deploy/monitoring"
	"github.com/velocor/velocor/internal/store"
	"go.yaml.in/yaml/v3"
)

// Tunable is a rule's adjustable value: its default appears in the shipped expression as
// Op+Default (e.g. "> 70"), and is replaced there.
type Tunable struct {
	Op      string  `json:"-"`
	Default float64 `json:"default"`
	Min     float64 `json:"min"`
	Max     float64 `json:"max"`
	Unit    string  `json:"unit"` // "celsius", "ratio"
	Label   string  `json:"label"`
}

var tunables = map[string]Tunable{
	"VelocorDiskHot":     {Op: "> ", Default: 70, Min: 40, Max: 95, Unit: "celsius", Label: "Temperature above"},
	"VelocorDiskWornOut": {Op: ">= ", Default: 0.9, Min: 0.5, Max: 1.5, Unit: "ratio", Label: "Wear at or above"},
}

// RuleInfo describes a shipped rule for the UI, with the administrator's setting.
type RuleInfo struct {
	Name       string            `json:"name"`
	Group      string            `json:"group"`
	Severity   string            `json:"severity"`
	Summary    string            `json:"summary"`
	DefaultFor string            `json:"default_for,omitempty"`
	Tunable    *Tunable          `json:"tunable,omitempty"`
	Setting    store.RuleSetting `json:"setting"`
}

type ruleFile struct {
	Groups []struct {
		Name  string           `yaml:"name"`
		Rules []map[string]any `yaml:"rules"`
	} `yaml:"groups"`
}

func parseShipped() (ruleFile, error) {
	var f ruleFile
	err := yaml.Unmarshal(rules.Rules, &f)
	return f, err
}

// Rules lists the shipped rules (the watchdog left out: it must always fire).
func Rules(settings map[string]store.RuleSetting) ([]RuleInfo, error) {
	f, err := parseShipped()
	if err != nil {
		return nil, err
	}
	var out []RuleInfo
	for _, g := range f.Groups {
		for _, r := range g.Rules {
			name, _ := r["alert"].(string)
			if name == "" || name == "VelocorWatchdog" {
				continue
			}
			info := RuleInfo{Name: name, Group: g.Name, Setting: settings[name]}
			if l, ok := r["labels"].(map[string]any); ok {
				info.Severity, _ = l["severity"].(string)
			}
			if a, ok := r["annotations"].(map[string]any); ok {
				info.Summary, _ = a["summary"].(string)
			}
			info.DefaultFor, _ = r["for"].(string)
			if t, ok := tunables[name]; ok {
				t := t
				info.Tunable = &t
			}
			out = append(out, info)
		}
	}
	return out, nil
}

// ValidateRuleSettings checks settings against the shipped rules.
func ValidateRuleSettings(settings map[string]store.RuleSetting) error {
	known, err := Rules(nil)
	if err != nil {
		return err
	}
	byName := map[string]RuleInfo{}
	for _, r := range known {
		byName[r.Name] = r
	}
	for name, s := range settings {
		r, ok := byName[name]
		if !ok {
			return fmt.Errorf("%w: no rule %q", store.ErrInvalidArgument, name)
		}
		if s.For != "" {
			d, err := time.ParseDuration(s.For)
			if err != nil || d < 0 || d > 7*24*time.Hour || d%time.Minute != 0 {
				return fmt.Errorf("%w: %s: delay must be whole minutes up to 7 days (e.g. 10m, 2h)", store.ErrInvalidArgument, name)
			}
		}
		if s.Threshold != nil {
			if r.Tunable == nil {
				return fmt.Errorf("%w: %s has no adjustable threshold", store.ErrInvalidArgument, name)
			}
			if v := *s.Threshold; v < r.Tunable.Min || v > r.Tunable.Max {
				return fmt.Errorf("%w: %s: threshold must be between %v and %v", store.ErrInvalidArgument, name, r.Tunable.Min, r.Tunable.Max)
			}
		}
	}
	return nil
}

// RenderRules returns the shipped rules with the settings applied: disabled rules removed,
// delays and thresholds replaced.
func RenderRules(settings map[string]store.RuleSetting) ([]byte, error) {
	if len(settings) == 0 {
		return rules.Rules, nil
	}
	f, err := parseShipped()
	if err != nil {
		return nil, err
	}
	for gi := range f.Groups {
		kept := f.Groups[gi].Rules[:0]
		for _, r := range f.Groups[gi].Rules {
			name, _ := r["alert"].(string)
			s, ok := settings[name]
			if !ok || name == "VelocorWatchdog" {
				kept = append(kept, r)
				continue
			}
			if s.Disabled {
				continue
			}
			if s.For != "" {
				d, _ := time.ParseDuration(s.For)
				if d == 0 {
					delete(r, "for")
				} else {
					r["for"] = promDuration(d)
				}
			}
			if t, ok := tunables[name]; ok && s.Threshold != nil {
				expr, _ := r["expr"].(string)
				old := t.Op + strconv.FormatFloat(t.Default, 'f', -1, 64)
				if !strings.Contains(expr, old) {
					return nil, fmt.Errorf("rule %s: expression has no %q to adjust", name, old)
				}
				r["expr"] = strings.Replace(expr, old, t.Op+strconv.FormatFloat(*s.Threshold, 'f', -1, 64), 1)
			}
			kept = append(kept, r)
		}
		f.Groups[gi].Rules = kept
	}
	out, err := yaml.Marshal(f)
	if err != nil {
		return nil, err
	}
	return append([]byte("# Shipped rules with this appliance's settings applied (written by velod).\n"), out...), nil
}

// promDuration writes d the way Prometheus does ("1h30m", "10m").
func promDuration(d time.Duration) string {
	h, m := int(d/time.Hour), int(d%time.Hour/time.Minute)
	switch {
	case h > 0 && m > 0:
		return fmt.Sprintf("%dh%dm", h, m)
	case h > 0:
		return fmt.Sprintf("%dh", h)
	default:
		return fmt.Sprintf("%dm", m)
	}
}
