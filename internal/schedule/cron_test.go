package schedule

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustParseCron(t *testing.T, expr string) cronExpr {
	t.Helper()
	c, err := parseCron(expr)
	if err != nil {
		t.Fatalf("parse %q: %v", expr, err)
	}
	return c
}

func mustLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("load %q: %v", name, err)
	}
	return loc
}

// The field syntax: wildcards, numbers, ranges, lists and steps, with the Vixie day rule.
func TestParseCron(t *testing.T) {
	cases := []struct {
		expr  string
		check func(cronExpr) bool
	}{
		{"* * * * *", func(c cronExpr) bool {
			return has(c.minute, 0) && has(c.minute, 59) && has(c.hour, 23) && !c.domRestricted
		}},
		{"5 4 * * *", func(c cronExpr) bool { return has(c.minute, 5) && !has(c.minute, 6) && has(c.hour, 4) }},
		{"*/15 * * * *", func(c cronExpr) bool {
			return has(c.minute, 0) && has(c.minute, 15) && has(c.minute, 30) && has(c.minute, 45) && !has(c.minute, 50)
		}},
		{"10-20/5 * * * *", func(c cronExpr) bool {
			return has(c.minute, 10) && has(c.minute, 15) && has(c.minute, 20) && !has(c.minute, 25)
		}},
		{"0,30 9,17 * * *", func(c cronExpr) bool {
			return has(c.minute, 0) && has(c.minute, 30) && has(c.hour, 9) && has(c.hour, 17) && !has(c.hour, 10)
		}},
		{"0 0 1-7 * 1", func(c cronExpr) bool { return c.domRestricted && c.dowRestricted && has(c.dom, 7) && has(c.dow, 1) }},
		{"0 0 * * 7", func(c cronExpr) bool { return has(c.dow, 0) && !has(c.dow, 7) && c.dowRestricted }},
		{"0 0 * * 0-7", func(c cronExpr) bool { return has(c.dow, 0) && has(c.dow, 6) }},
	}
	for _, tc := range cases {
		c := mustParseCron(t, tc.expr)
		if !tc.check(c) {
			t.Errorf("%q parsed to minute=%b hour=%b dom=%b dow=%b", tc.expr, c.minute, c.hour, c.dom, c.dow)
		}
	}

	for _, bad := range []string{
		"", "* * * *", "* * * * * *", "60 * * * *", "* 24 * * *", "* * 0 * *", "* * 32 * *",
		"* * * 13 *", "* * * * 8", "*/0 * * * *", "5-1 * * * *", "a * * * *", "1,,2 * * * *",
		"* * * * MON",
		"5/15 * * * *",
	} {
		if _, err := parseCron(bad); err == nil {
			t.Errorf("%q parsed, want an error", bad)
		} else if !errors.Is(err, ErrInvalid) {
			t.Errorf("%q: err = %v, want ErrInvalid", bad, err)
		}
	}
}

// next walks a plain expression minute by minute and honours the OR rule of the day fields.
func TestCronNext(t *testing.T) {
	utc := time.UTC
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, utc)
	cases := []struct {
		expr string
		from time.Time
		want time.Time
	}{
		{"* * * * *", base, base.Add(time.Minute)},
		{"*/15 * * * *", base, base.Add(15 * time.Minute)},
		{"0 * * * *", base, base.Add(time.Hour)},
		{"0 0 * * *", base, time.Date(2026, 11, 2, 0, 0, 0, 0, utc)},
		{"30 9 * * *", base, base.Add(30 * time.Minute)},
		{"30 9 * * *", base.Add(time.Minute), base.Add(30 * time.Minute)},
		{"30 9 * * *", base.Add(30 * time.Minute), time.Date(2026, 11, 2, 9, 30, 0, 0, utc)},
		// 1 November 2026 is a Sunday; "1st of the month or a Monday" fires on either.
		{"0 12 1 * 1", time.Date(2026, 11, 1, 13, 0, 0, 0, utc), time.Date(2026, 11, 2, 12, 0, 0, 0, utc)},
		{"0 12 1 * 1", time.Date(2026, 11, 2, 13, 0, 0, 0, utc), time.Date(2026, 11, 9, 12, 0, 0, 0, utc)},
		// Only the day of month is restricted, so the weekday does not widen it.
		{"0 12 1 * *", time.Date(2026, 11, 2, 0, 0, 0, 0, utc), time.Date(2026, 12, 1, 12, 0, 0, 0, utc)},
	}
	for _, tc := range cases {
		got, err := mustParseCron(t, tc.expr).next(tc.from, utc)
		if err != nil {
			t.Errorf("%q from %s: %v", tc.expr, tc.from, err)
			continue
		}
		if !got.Equal(tc.want) {
			t.Errorf("%q from %s = %s, want %s", tc.expr, tc.from.Format(time.RFC3339), got.Format(time.RFC3339), tc.want.Format(time.RFC3339))
		}
	}
}

// A cron expression is read in its own zone: the same expression fires at the zone's wall clock.
func TestCronNextInTimezone(t *testing.T) {
	tokyo := mustLoc(t, "Asia/Tokyo")
	expr := mustParseCron(t, "30 9 * * *")
	from := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC) // 09:00 in Tokyo
	got, err := expr.next(from, tokyo)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 11, 1, 9, 30, 0, 0, tokyo)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Hour() != 9 || got.Minute() != 30 {
		t.Fatalf("next in zone = %s", got.Format(time.RFC3339))
	}
}

// A local time that does not exist (the spring-forward gap) is skipped, not shifted: the 02:30 task
// does not fire on the night 02:00 becomes 03:00.
func TestCronSkipsNonexistentLocalTime(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	// 2026-03-08: 02:00 EST becomes 03:00 EDT.
	from := time.Date(2026, 3, 8, 0, 0, 0, 0, ny)
	got, err := mustParseCron(t, "30 2 * * *").next(from, ny)
	if err != nil {
		t.Fatal(err)
	}
	want := time.Date(2026, 3, 9, 2, 30, 0, 0, ny)
	if !got.Equal(want) {
		t.Fatalf("next = %s, want %s (02:30 does not exist on 8 March)", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if got.Hour() != 2 || got.Minute() != 30 {
		t.Fatalf("next local time = %s", got.Format(time.RFC3339))
	}

	// A time that exists that night fires at its zone's wall clock (03:00 EDT is 07:00 UTC).
	got, err = mustParseCron(t, "0 3 * * *").next(from, ny)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 3, 8, 7, 0, 0, 0, time.UTC); !got.Equal(want) {
		t.Fatalf("03:00 on 8 March = %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// A match inside the spring-forward gap skips only the times that do not exist: the next match that
// exists the same night fires, not the next day's first (RT-state-7). Lord Howe moves its clock by 30
// minutes, so the next valid time is not always the next hour.
func TestCronSpringForwardResumesAtTheNextValidTime(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	lordHowe := mustLoc(t, "Australia/Lord_Howe")
	for _, tc := range []struct {
		expr string
		from time.Time
		want time.Time
	}{
		// 2026-03-08 in New York: 02:00 EST becomes 03:00 EDT.
		{"0 * * * *", time.Date(2026, 3, 8, 1, 50, 0, 0, ny), time.Date(2026, 3, 8, 3, 0, 0, 0, ny)},
		{"*/15 * * * *", time.Date(2026, 3, 8, 1, 50, 0, 0, ny), time.Date(2026, 3, 8, 3, 0, 0, 0, ny)},
		{"0 2,9 * * *", time.Date(2026, 3, 8, 1, 50, 0, 0, ny), time.Date(2026, 3, 8, 9, 0, 0, 0, ny)},
		{"30 2 * * *", time.Date(2026, 3, 8, 1, 50, 0, 0, ny), time.Date(2026, 3, 9, 2, 30, 0, 0, ny)},
		// 2026-10-04 on Lord Howe Island: 02:00 becomes 02:30.
		{"*/15 * * * *", time.Date(2026, 10, 4, 1, 50, 0, 0, lordHowe), time.Date(2026, 10, 4, 2, 30, 0, 0, lordHowe)},
	} {
		got, err := mustParseCron(t, tc.expr).next(tc.from, tc.from.Location())
		if err != nil {
			t.Fatalf("%q: %v", tc.expr, err)
		}
		if !got.Equal(tc.want) {
			t.Errorf("%q after %s = %s, want %s", tc.expr, tc.from.Format(time.RFC3339),
				got.In(tc.from.Location()).Format(time.RFC3339), tc.want.Format(time.RFC3339))
		}
	}
}

// A local time that occurs twice (the fall-back hour) fires once: the second 01:30 is not a new slot.
func TestCronFiresOnceInRepeatedHour(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	expr := mustParseCron(t, "30 1 * * *")
	// 2026-11-01: 02:00 EDT becomes 01:00 EST, so 01:30 happens twice.
	first, err := expr.next(time.Date(2026, 11, 1, 0, 0, 0, 0, ny), ny)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 1, 1, 30, 0, 0, ny); !first.Equal(want) {
		t.Fatalf("first = %s, want %s", first.Format(time.RFC3339), want.Format(time.RFC3339))
	}
	if _, offset := first.Zone(); offset != -4*3600 {
		t.Fatalf("first occurrence offset = %d, want EDT (-4h)", offset)
	}
	second, err := expr.next(first, ny)
	if err != nil {
		t.Fatal(err)
	}
	if want := time.Date(2026, 11, 2, 1, 30, 0, 0, ny); !second.Equal(want) {
		t.Fatalf("second = %s, want %s (the repeated 01:30 is not a new slot)", second.Format(time.RFC3339), want.Format(time.RFC3339))
	}
}

// A cron expression that can never match is refused with a clear error instead of searching forever.
func TestCronNoNextFireIsBounded(t *testing.T) {
	from := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	_, err := mustParseCron(t, "0 0 30 2 *").next(from, time.UTC)
	if !errors.Is(err, ErrNoNextFire) {
		t.Fatalf("err = %v, want ErrNoNextFire", err)
	}
	if !strings.Contains(err.Error(), "0 0 30 2 *") {
		t.Fatalf("err = %v, want it to name the expression", err)
	}
}

// The task-level cron trigger stores its zone and computes the next slot from the expression; a bad
// zone or expression is refused at create time.
func TestCronTaskTrigger(t *testing.T) {
	ny := mustLoc(t, "America/New_York")
	task := Task{
		ID: "schedule_cron", Name: "morning", Kind: KindShell,
		Shell:        &ShellPayload{Command: "make"},
		Trigger:      Trigger{Kind: TriggerCron, Cron: "30 9 * * 1-5", Timezone: "America/New_York"},
		MissedPolicy: MissedSkip,
	}
	if err := validate(task, time.Minute); err != nil {
		t.Fatalf("validate: %v", err)
	}
	now := time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC) // Sunday 07:00 in New York
	next, err := nextSlot(task, now)
	if err != nil {
		t.Fatal(err)
	}
	// Monday 09:30 EST is 14:30 UTC.
	if want := time.Date(2026, 11, 2, 14, 30, 0, 0, time.UTC); next == nil || !next.Equal(want) {
		t.Fatalf("next = %v, want %s", next, want.Format(time.RFC3339))
	}
	if next.In(ny).Hour() != 9 {
		t.Fatalf("next in New York = %s", next.In(ny).Format(time.RFC3339))
	}

	bad := []Trigger{
		{Kind: TriggerCron, Cron: "0 0 30 2 *", Timezone: "UTC"},
		{Kind: TriggerCron, Cron: "30 9 * * *", Timezone: "Mars/Olympus"},
		{Kind: TriggerCron, Cron: ""},
		{Kind: TriggerCron, Cron: "30 9 * * *", Every: time.Minute},
		{Kind: TriggerInterval, Every: time.Hour, Cron: "30 9 * * *"},
	}
	for _, trigger := range bad {
		broken := task
		broken.Trigger = trigger
		if err := validate(broken, time.Minute); err == nil {
			t.Errorf("trigger %+v validated, want an error", trigger)
		}
	}
}
