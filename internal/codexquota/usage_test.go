package codexquota

import (
	"testing"
	"time"
)

func TestParseUsageExtractsFiveHourAndWeeklyWindows(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{
		"rate_limit": {
			"primary_window": {
				"limit_window_seconds": 18000,
				"used_percent": 100,
				"reset_at": 1700003600
			},
			"secondary_window": {
				"limit_window_seconds": 604800,
				"used_percent": 45,
				"reset_at": 1700600000
			}
		}
	}`)

	snapshot := ParseUsage(body, now)
	if !snapshot.FiveHour.Present || !snapshot.FiveHour.Exhausted {
		t.Fatalf("expected exhausted five-hour window, got %+v", snapshot.FiveHour)
	}
	if snapshot.FiveHour.RecoverAt.Unix() != 1_700_003_600 {
		t.Fatalf("five-hour recover_at = %v", snapshot.FiveHour.RecoverAt)
	}
	if !snapshot.Weekly.Present || snapshot.Weekly.Exhausted {
		t.Fatalf("expected non-exhausted weekly window, got %+v", snapshot.Weekly)
	}
}

func TestParseUsageFallsBackToParentFlags(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	body := []byte(`{
		"rateLimit": {
			"allowed": false,
			"limitReached": true,
			"primaryWindow": {
				"limitWindowSeconds": 18000,
				"resetAfterSeconds": 60
			}
		}
	}`)

	snapshot := ParseUsage(body, now)
	if !snapshot.FiveHour.Present || !snapshot.FiveHour.Exhausted {
		t.Fatalf("expected exhausted five-hour fallback window, got %+v", snapshot.FiveHour)
	}
	if got := snapshot.FiveHour.RecoverAt.Sub(now); got < 59*time.Second || got > 61*time.Second {
		t.Fatalf("unexpected recover duration: %v", got)
	}
}

func TestSnapshotExhaustedWindowReturnsLatestRecoverAt(t *testing.T) {
	snapshot := Snapshot{
		FiveHour: Window{Present: true, Exhausted: true, RecoverAt: time.Unix(10, 0)},
		Weekly:   Window{Present: true, Exhausted: true, RecoverAt: time.Unix(20, 0)},
	}

	window, ok := snapshot.ExhaustedWindow()
	if !ok {
		t.Fatal("expected exhausted window")
	}
	if window.RecoverAt.Unix() != 20 {
		t.Fatalf("recover_at = %v, want 20", window.RecoverAt.Unix())
	}
}
