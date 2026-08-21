package activationwindow

import (
	"context"
	"strings"
	"testing"
	"time"
)

func TestEvaluateDayAndOvernightWindows(t *testing.T) {
	tests := []struct {
		name       string
		expression string
		instant    string
		open       bool
	}{
		{name: "weekday-open", expression: "window.v1;tz=America/Chicago;days=Mon,Tue,Wed,Thu,Fri;start=04:00;end=05:00", instant: "2026-08-21T04:30:00-05:00", open: true},
		{name: "end-exclusive", expression: "window.v1;tz=America/Chicago;days=Mon,Tue,Wed,Thu,Fri;start=04:00;end=05:00", instant: "2026-08-21T05:00:00-05:00", open: false},
		{name: "weekend-closed", expression: "window.v1;tz=America/Chicago;days=Mon,Tue,Wed,Thu,Fri;start=04:00;end=05:00", instant: "2026-08-22T04:30:00-05:00", open: false},
		{name: "overnight-start", expression: "window.v1;tz=UTC;days=Fri;start=23:00;end=01:00", instant: "2026-08-21T23:30:00Z", open: true},
		{name: "overnight-next-day", expression: "window.v1;tz=UTC;days=Fri;start=23:00;end=01:00", instant: "2026-08-22T00:30:00Z", open: true},
		{name: "overnight-after-end", expression: "window.v1;tz=UTC;days=Fri;start=23:00;end=01:00", instant: "2026-08-22T01:00:00Z", open: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			instant, err := time.Parse(time.RFC3339, tc.instant)
			if err != nil {
				t.Fatal(err)
			}
			open, err := Evaluate(context.Background(), tc.expression, instant)
			if err != nil || open != tc.open {
				t.Fatalf("open=%v want=%v err=%v", open, tc.open, err)
			}
		})
	}
}

func TestParseRejectsAmbiguousOrNonCanonicalWindows(t *testing.T) {
	invalid := []string{
		"04:00-05:00",
		"window.v1;tz=Local;days=*;start=04:00;end=05:00",
		"window.v1;tz=Not/AZone;days=*;start=04:00;end=05:00",
		"window.v1;tz=UTC;days=Fri,Mon;start=04:00;end=05:00",
		"window.v1;tz=UTC;days=Mon,Mon;start=04:00;end=05:00",
		"window.v1;tz=UTC;days=*;start=04:00;end=04:00",
		"window.v1;tz=UTC;days=*;start=24:00;end=05:00",
	}
	for _, expression := range invalid {
		t.Run(strings.ReplaceAll(expression, "/", "_"), func(t *testing.T) {
			if _, err := Parse(expression); err == nil {
				t.Fatalf("accepted %q", expression)
			}
		})
	}
}

func TestEvaluateHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := Evaluate(ctx, "window.v1;tz=UTC;days=*;start=04:00;end=05:00", time.Now()); err == nil {
		t.Fatal("cancelled evaluation succeeded")
	}
}
