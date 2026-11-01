package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	// The IANA database is embedded, so a cron task's timezone resolves the same way on every host
	// (a Windows or a slim container image may carry no zoneinfo files).
	_ "time/tzdata"
)

// ErrNoNextFire reports a cron expression that matches no time within the search bound. It is what a
// schedule such as "0 0 30 2 *" (30 February) gets instead of an endless search.
var ErrNoNextFire = errors.New("cron expression has no fire time within five years")

// cronSearchYears bounds the search for the next fire time: no expression needs longer, and a bound
// keeps a never-matching one from spinning.
const cronSearchYears = 5

// cronExpr is a parsed standard five-field cron expression: minute, hour, day of month, month, day of
// week. The day fields follow Vixie cron: when both are restricted (neither is "*"), a day matches if
// either matches.
type cronExpr struct {
	minute uint64 // bits 0..59
	hour   uint64 // bits 0..23
	dom    uint64 // bits 1..31
	month  uint64 // bits 1..12
	dow    uint64 // bits 0..6, Sunday 0

	domRestricted bool
	dowRestricted bool
	text          string
}

// parseCron parses a five-field expression. Supported per field: "*", a number, a range "a-b", a list
// "a,b,c" of numbers and ranges, and a step "/n" on "*" or a range ("*/5", "1-30/2"). Day of week
// accepts 0-7, where both 0 and 7 are Sunday.
func parseCron(expr string) (cronExpr, error) {
	fields := strings.Fields(expr)
	if len(fields) != 5 {
		return cronExpr{}, fmt.Errorf("%w: a cron expression has 5 fields (minute hour day-of-month month day-of-week), got %d", ErrInvalid, len(fields))
	}
	var (
		c   cronExpr
		err error
	)
	c.text = strings.Join(fields, " ")
	if c.minute, _, err = cronField(fields[0], 0, 59); err != nil {
		return cronExpr{}, fmt.Errorf("%w: minute field: %v", ErrInvalid, err)
	}
	if c.hour, _, err = cronField(fields[1], 0, 23); err != nil {
		return cronExpr{}, fmt.Errorf("%w: hour field: %v", ErrInvalid, err)
	}
	if c.dom, c.domRestricted, err = cronField(fields[2], 1, 31); err != nil {
		return cronExpr{}, fmt.Errorf("%w: day-of-month field: %v", ErrInvalid, err)
	}
	if c.month, _, err = cronField(fields[3], 1, 12); err != nil {
		return cronExpr{}, fmt.Errorf("%w: month field: %v", ErrInvalid, err)
	}
	if c.dow, c.dowRestricted, err = cronField(fields[4], 0, 7); err != nil {
		return cronExpr{}, fmt.Errorf("%w: day-of-week field: %v", ErrInvalid, err)
	}
	if c.dow&(1<<7) != 0 {
		c.dow = (c.dow &^ (1 << 7)) | 1 // 7 is Sunday, like 0
	}
	return c, nil
}

// cronField parses one field into a bitset over min..max.
func cronField(text string, min, max int) (mask uint64, restricted bool, err error) {
	if text == "" {
		return 0, false, errors.New("empty field")
	}
	for _, item := range strings.Split(text, ",") {
		base, stepText, hasStep := strings.Cut(item, "/")
		step := 1
		if hasStep {
			if base != "*" && !strings.Contains(base, "-") {
				return 0, false, fmt.Errorf("a step needs * or a range before /, not %q", base)
			}
			n, convErr := strconv.Atoi(stepText)
			if convErr != nil || n < 1 {
				return 0, false, fmt.Errorf("step %q must be a positive integer", stepText)
			}
			step = n
		}
		lo, hi := min, max
		switch {
		case base == "*":
			restricted = restricted || hasStep
		default:
			restricted = true
			from, to, isRange := strings.Cut(base, "-")
			lo, err = cronNumber(from, min, max)
			if err != nil {
				return 0, false, err
			}
			hi = lo
			if isRange {
				if hi, err = cronNumber(to, min, max); err != nil {
					return 0, false, err
				}
				if hi < lo {
					return 0, false, fmt.Errorf("range %q ends before it starts", base)
				}
			}
		}
		for v := lo; v <= hi; v += step {
			mask |= 1 << uint(v)
		}
	}
	return mask, restricted, nil
}

func cronNumber(text string, min, max int) (int, error) {
	n, err := strconv.Atoi(text)
	if err != nil {
		return 0, fmt.Errorf("%q is not a number", text)
	}
	if n < min || n > max {
		return 0, fmt.Errorf("%d is outside %d..%d", n, min, max)
	}
	return n, nil
}

// has reports whether the bitset holds v.
func has(mask uint64, v int) bool {
	if v < 0 || v > 63 {
		return false
	}
	return mask&(1<<uint(v)) != 0
}

// next returns the first time strictly after `after` that matches the expression, in loc. A local time
// that does not exist (the spring-forward gap) is skipped; a local time that occurs twice (the
// fall-back hour) fires once, at its first occurrence.
//
// The search walks a wall-clock cursor (a noon-anchored day plus hour and minute), never a reconstructed
// instant: normalizing a nonexistent local time can move it either way, and a cursor built from it can
// stall. A candidate is accepted only when the instant it denotes reads back the same local fields.
func (c cronExpr) next(after time.Time, loc *time.Location) (time.Time, error) {
	if loc == nil {
		loc = time.UTC
	}
	start := after.In(loc)
	day := time.Date(start.Year(), start.Month(), start.Day(), 12, 0, 0, 0, loc)
	h, m := start.Hour(), start.Minute()
	nextMinute := func() {
		if m++; m > 59 {
			m = 0
			if h++; h > 23 {
				h = 0
				day = day.AddDate(0, 0, 1)
			}
		}
	}
	nextMinute()
	limit := start.AddDate(cronSearchYears, 0, 0)
	startY, startMo, startD := start.Date()
	startH, startM := start.Hour(), start.Minute()
	for {
		if day.Sub(start) > limit.Sub(start) {
			return time.Time{}, fmt.Errorf("%w: %s", ErrNoNextFire, c.text)
		}
		if !has(c.month, int(day.Month())) {
			day = time.Date(day.Year(), day.Month()+1, 1, 12, 0, 0, 0, loc)
			h, m = 0, 0
			continue
		}
		if !c.dayMatches(day) {
			day, h, m = day.AddDate(0, 0, 1), 0, 0
			continue
		}
		if !has(c.hour, h) {
			if h++; h > 23 {
				day, h, m = day.AddDate(0, 0, 1), 0, 0
			} else {
				m = 0
			}
			continue
		}
		if !has(c.minute, m) {
			nextMinute()
			continue
		}
		inst := time.Date(day.Year(), day.Month(), day.Day(), h, m, 0, 0, loc)
		if iy, imo, id := inst.Date(); iy != day.Year() || imo != day.Month() || id != day.Day() ||
			inst.Hour() != h || inst.Minute() != m {
			// The local time does not exist (the hour the clock skips): the whole day after it is
			// still to come, so continue with the next day.
			day, h, m = day.AddDate(0, 0, 1), 0, 0
			continue
		}
		if !inst.After(start) {
			nextMinute()
			continue
		}
		if iy, imo, id := inst.Date(); iy == startY && imo == startMo && id == startD && inst.Hour() == startH && inst.Minute() == startM {
			// The same wall clock as the previous occurrence: the repeated hour of a fall-back
			// night, whose slot already fired.
			nextMinute()
			continue
		}
		return inst, nil
	}
}

// dayMatches applies the Vixie rule: with both day fields restricted, either may match; with one
// restricted, that one decides; with neither, every day matches. day is anchored at noon, so its
// weekday and date are unambiguous in any zone.
func (c cronExpr) dayMatches(day time.Time) bool {
	y, mo, d := day.Date()
	dom := has(c.dom, d)
	dow := has(c.dow, int(time.Date(y, mo, d, 12, 0, 0, 0, time.UTC).Weekday()))
	switch {
	case c.domRestricted && c.dowRestricted:
		return dom || dow
	case c.domRestricted:
		return dom
	case c.dowRestricted:
		return dow
	}
	return true
}

// loadTimezone resolves a task's IANA zone; "" is UTC.
func loadTimezone(name string) (*time.Location, error) {
	if name == "" || name == "UTC" {
		return time.UTC, nil
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, fmt.Errorf("%w: timezone %q is not an IANA zone: %v", ErrInvalid, name, err)
	}
	return loc, nil
}
