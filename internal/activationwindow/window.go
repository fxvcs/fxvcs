// Package activationwindow parses and evaluates the portable v1 target
// activation-window expression.
package activationwindow

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
	_ "time/tzdata" // Embed IANA data so host package drift cannot change policy evaluation.
)

const Prefix = "window.v1"

// Window is a parsed weekly wall-clock window. For an overnight window, Days
// identifies the day on which the window starts.
type Window struct {
	Location    *time.Location
	LocationID  string
	Days        [7]bool
	StartMinute int
	EndMinute   int
}

// Parse accepts exactly:
// window.v1;tz=<IANA>;days=<*|Mon,Tue,...>;start=HH:MM;end=HH:MM
func Parse(expression string) (Window, error) {
	parts := strings.Split(expression, ";")
	if len(parts) != 5 || parts[0] != Prefix {
		return Window{}, errors.New("activation window: expected window.v1;tz=<IANA>;days=<*|Mon,Tue,...>;start=HH:MM;end=HH:MM")
	}
	tz, err := exactField(parts[1], "tz")
	if err != nil {
		return Window{}, err
	}
	daysText, err := exactField(parts[2], "days")
	if err != nil {
		return Window{}, err
	}
	startText, err := exactField(parts[3], "start")
	if err != nil {
		return Window{}, err
	}
	endText, err := exactField(parts[4], "end")
	if err != nil {
		return Window{}, err
	}
	location, err := time.LoadLocation(tz)
	if err != nil || tz == "Local" {
		return Window{}, fmt.Errorf("activation window: invalid explicit IANA timezone %q", tz)
	}
	days, err := parseDays(daysText)
	if err != nil {
		return Window{}, err
	}
	start, err := parseClock(startText)
	if err != nil {
		return Window{}, fmt.Errorf("activation window: invalid start: %w", err)
	}
	end, err := parseClock(endText)
	if err != nil {
		return Window{}, fmt.Errorf("activation window: invalid end: %w", err)
	}
	if start == end {
		return Window{}, errors.New("activation window: start and end must differ; use days=* for an every-day window")
	}
	return Window{Location: location, LocationID: tz, Days: days, StartMinute: start, EndMinute: end}, nil
}

func exactField(part, name string) (string, error) {
	prefix := name + "="
	if !strings.HasPrefix(part, prefix) || len(part) == len(prefix) {
		return "", fmt.Errorf("activation window: expected non-empty %s field", name)
	}
	return strings.TrimPrefix(part, prefix), nil
}

func parseClock(value string) (int, error) {
	if len(value) != 5 || value[2] != ':' {
		return 0, fmt.Errorf("expected HH:MM, got %q", value)
	}
	hour, hourErr := strconv.Atoi(value[:2])
	minute, minuteErr := strconv.Atoi(value[3:])
	if hourErr != nil || minuteErr != nil || hour < 0 || hour > 23 || minute < 0 || minute > 59 {
		return 0, fmt.Errorf("expected 00:00 through 23:59, got %q", value)
	}
	return hour*60 + minute, nil
}

func parseDays(value string) ([7]bool, error) {
	var out [7]bool
	if value == "*" {
		for i := range out {
			out[i] = true
		}
		return out, nil
	}
	aliases := map[string]time.Weekday{"Sun": time.Sunday, "Mon": time.Monday, "Tue": time.Tuesday, "Wed": time.Wednesday, "Thu": time.Thursday, "Fri": time.Friday, "Sat": time.Saturday}
	previous := -1
	for _, token := range strings.Split(value, ",") {
		day, ok := aliases[token]
		if !ok {
			return out, fmt.Errorf("activation window: invalid day %q", token)
		}
		index := int(day)
		if out[index] {
			return out, fmt.Errorf("activation window: duplicate day %q", token)
		}
		// Canonical ordering is Mon..Sun, matching operator-facing calendars.
		order := (index + 6) % 7
		if order <= previous {
			return out, errors.New("activation window: days must be in canonical Mon,Tue,Wed,Thu,Fri,Sat,Sun order")
		}
		previous = order
		out[index] = true
	}
	return out, nil
}

// Open reports whether the instant is inside the window. Wall-clock
// comparison intentionally makes skipped DST minutes closed and repeated DST
// minutes open for both occurrences. End is exclusive.
func (w Window) Open(now time.Time) bool {
	local := now.In(w.Location)
	minute := local.Hour()*60 + local.Minute()
	day := int(local.Weekday())
	if w.StartMinute < w.EndMinute {
		return w.Days[day] && minute >= w.StartMinute && minute < w.EndMinute
	}
	previousDay := (day + 6) % 7
	return (w.Days[day] && minute >= w.StartMinute) || (w.Days[previousDay] && minute < w.EndMinute)
}

// Evaluate matches app.ApplyDependencies.EvaluateActivationWindow.
func Evaluate(ctx context.Context, expression string, now time.Time) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	window, err := Parse(expression)
	if err != nil {
		return false, err
	}
	return window.Open(now), nil
}
