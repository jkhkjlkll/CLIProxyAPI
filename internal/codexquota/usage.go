package codexquota

import (
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"time"
)

const (
	fiveHourWindowSeconds = 5 * 60 * 60
	weeklyWindowSeconds   = 7 * 24 * 60 * 60
)

// Snapshot captures the quota windows we care about for proactive routing.
type Snapshot struct {
	FiveHour Window
	Weekly   Window
}

// Window captures the usage state for one rolling quota window.
type Window struct {
	Present     bool
	Exhausted   bool
	RecoverAt   time.Time
	UsedPercent float64
}

// ExhaustedWindow returns the latest active exhausted window reset time, if any.
func (s Snapshot) ExhaustedWindow() (Window, bool) {
	var (
		best  Window
		found bool
	)
	for _, candidate := range []Window{s.FiveHour, s.Weekly} {
		if !candidate.Present || !candidate.Exhausted {
			continue
		}
		if !found {
			best = candidate
			found = true
			continue
		}
		if best.RecoverAt.IsZero() || candidate.RecoverAt.After(best.RecoverAt) {
			best = candidate
		}
	}
	return best, found
}

// ParseUsage extracts 5h and weekly quota windows from the Codex usage payload.
func ParseUsage(body []byte, now time.Time) Snapshot {
	root := parseObject(body)
	if root == nil {
		return Snapshot{}
	}
	rateLimit := objectValue(root, "rate_limit", "rateLimit")
	if rateLimit == nil {
		return Snapshot{}
	}

	primary := objectValue(rateLimit, "primary_window", "primaryWindow")
	secondary := objectValue(rateLimit, "secondary_window", "secondaryWindow")
	fiveHourObj, weeklyObj := splitWindows(primary, secondary)

	return Snapshot{
		FiveHour: parseWindow(fiveHourObj, rateLimit, now),
		Weekly:   parseWindow(weeklyObj, rateLimit, now),
	}
}

func splitWindows(primary, secondary map[string]any) (map[string]any, map[string]any) {
	var fiveHour map[string]any
	var weekly map[string]any
	var weeklyFromPrimary bool
	var fiveHourFromSecondary bool

	for idx, candidate := range []map[string]any{primary, secondary} {
		if candidate == nil {
			continue
		}
		fromPrimary := idx == 0
		secs, ok := intValue(candidate["limit_window_seconds"])
		if !ok {
			secs, ok = intValue(candidate["limitWindowSeconds"])
		}
		if ok {
			switch secs {
			case fiveHourWindowSeconds:
				if fiveHour == nil {
					fiveHour = candidate
					fiveHourFromSecondary = !fromPrimary
				}
			case weeklyWindowSeconds:
				if weekly == nil {
					weekly = candidate
					weeklyFromPrimary = fromPrimary
				}
			}
		}
	}

	if fiveHour == nil && primary != nil && !weeklyFromPrimary {
		fiveHour = primary
	}
	if weekly == nil && secondary != nil && !fiveHourFromSecondary {
		weekly = secondary
	}

	return fiveHour, weekly
}

func parseWindow(windowObj, parent map[string]any, now time.Time) Window {
	if windowObj == nil {
		return Window{}
	}
	usedPercent, ok := floatValue(windowObj["used_percent"])
	if !ok {
		usedPercent, ok = floatValue(windowObj["usedPercent"])
	}
	recoverAt := resetFromWindow(windowObj, now)
	if !ok {
		limitReached, hasLimitReached := boolValue(parent["limit_reached"])
		if !hasLimitReached {
			limitReached, hasLimitReached = boolValue(parent["limitReached"])
		}
		allowed, hasAllowed := boolValue(parent["allowed"])
		if recoverAt.IsZero() {
			// Without a reset timestamp we cannot safely pre-cool down the auth.
			return Window{}
		}
		if (hasLimitReached && limitReached) || (hasAllowed && !allowed) {
			usedPercent = 100
			ok = true
		}
	}
	if !ok {
		return Window{}
	}

	if usedPercent < 0 {
		usedPercent = 0
	}
	if usedPercent > 100 {
		usedPercent = 100
	}

	return Window{
		Present:     true,
		Exhausted:   usedPercent >= 100 || (!recoverAt.IsZero() && math.Abs(usedPercent-100) < 0.001),
		RecoverAt:   recoverAt,
		UsedPercent: usedPercent,
	}
}

func resetFromWindow(window map[string]any, now time.Time) time.Time {
	if window == nil {
		return time.Time{}
	}
	if resetAt, ok := intValue(window["reset_at"]); ok && resetAt > 0 {
		return time.Unix(int64(resetAt), 0)
	}
	if resetAt, ok := intValue(window["resetAt"]); ok && resetAt > 0 {
		return time.Unix(int64(resetAt), 0)
	}
	if resetAfter, ok := floatValue(window["reset_after_seconds"]); ok && resetAfter > 0 {
		return now.Add(time.Duration(resetAfter * float64(time.Second)))
	}
	if resetAfter, ok := floatValue(window["resetAfterSeconds"]); ok && resetAfter > 0 {
		return now.Add(time.Duration(resetAfter * float64(time.Second)))
	}
	return time.Time{}
}

func parseObject(body []byte) map[string]any {
	if len(body) == 0 {
		return nil
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return nil
	}
	return root
}

func objectValue(node map[string]any, keys ...string) map[string]any {
	if node == nil {
		return nil
	}
	for _, key := range keys {
		raw, ok := node[key]
		if !ok || raw == nil {
			continue
		}
		if out, ok := raw.(map[string]any); ok {
			return out
		}
	}
	return nil
}

func floatValue(raw any) (float64, bool) {
	switch typed := raw.(type) {
	case float64:
		return typed, true
	case float32:
		return float64(typed), true
	case int:
		return float64(typed), true
	case int32:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case uint:
		return float64(typed), true
	case uint32:
		return float64(typed), true
	case uint64:
		return float64(typed), true
	case json.Number:
		if v, err := typed.Float64(); err == nil {
			return v, true
		}
	case string:
		cleaned := strings.TrimSpace(typed)
		if cleaned == "" {
			return 0, false
		}
		cleaned = strings.Map(func(r rune) rune {
			switch {
			case r >= '0' && r <= '9':
				return r
			case r == '.':
				return r
			default:
				return -1
			}
		}, cleaned)
		if cleaned == "" {
			return 0, false
		}
		v, err := strconv.ParseFloat(cleaned, 64)
		if err == nil {
			return v, true
		}
	}
	return 0, false
}

func intValue(raw any) (int, bool) {
	switch typed := raw.(type) {
	case int:
		return typed, true
	case int32:
		return int(typed), true
	case int64:
		return int(typed), true
	case uint:
		return int(typed), true
	case uint32:
		return int(typed), true
	case uint64:
		return int(typed), true
	case float64:
		return int(typed), true
	case json.Number:
		if v, err := typed.Int64(); err == nil {
			return int(v), true
		}
	case string:
		cleaned := strings.TrimSpace(typed)
		if cleaned == "" {
			return 0, false
		}
		v, err := strconv.Atoi(cleaned)
		if err == nil {
			return v, true
		}
	}
	return 0, false
}

func boolValue(raw any) (bool, bool) {
	switch typed := raw.(type) {
	case bool:
		return typed, true
	case string:
		cleaned := strings.TrimSpace(typed)
		if cleaned == "" {
			return false, false
		}
		v, err := strconv.ParseBool(cleaned)
		if err == nil {
			return v, true
		}
	}
	return false, false
}
