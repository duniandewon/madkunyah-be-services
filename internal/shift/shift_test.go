package shift

import (
	"net/http/httptest"
	"testing"
	"time"
)

// AC-1: the business date follows the outlet's cutoff and timezone.
func TestBusinessDate(t *testing.T) {
	jakarta, err := time.LoadLocation("Asia/Jakarta") // UTC+7
	if err != nil {
		t.Fatal(err)
	}
	jayapura, err := time.LoadLocation("Asia/Jayapura") // UTC+9
	if err != nil {
		t.Fatal(err)
	}
	at := func(loc *time.Location, day, hour, minute int) time.Time {
		return time.Date(2026, time.November, day, hour, minute, 0, 0, loc).UTC()
	}
	for _, tt := range []struct {
		name   string
		at     time.Time
		loc    *time.Location
		cutoff int
		want   string
	}{
		{"morning, after cutoff", at(jakarta, 12, 9, 0), jakarta, 4 * 60, "2026-11-12"},
		{"before cutoff", at(jakarta, 12, 2, 30), jakarta, 4 * 60, "2026-11-11"},
		{"one minute before cutoff", at(jakarta, 12, 3, 59), jakarta, 4 * 60, "2026-11-11"},
		{"exactly at cutoff", at(jakarta, 12, 4, 0), jakarta, 4 * 60, "2026-11-12"},
		{"just after midnight", at(jakarta, 12, 0, 5), jakarta, 4 * 60, "2026-11-11"},
		{"just before midnight", at(jakarta, 12, 23, 55), jakarta, 4 * 60, "2026-11-12"},
		{"midnight cutoff", at(jakarta, 12, 0, 0), jakarta, 0, "2026-11-12"},
		{"non-default cutoff, before", at(jakarta, 12, 5, 59), jakarta, 6 * 60, "2026-11-11"},
		{"non-default cutoff, after", at(jakarta, 12, 6, 0), jakarta, 6 * 60, "2026-11-12"},
		// 20:00 UTC on the 11th is 05:00 on the 12th in Jayapura but 03:00
		// in Jakarta: the same instant, two business dates.
		{"other timezone, after cutoff", time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC), jayapura, 4 * 60, "2026-11-12"},
		{"same instant in Jakarta", time.Date(2026, 11, 11, 20, 0, 0, 0, time.UTC), jakarta, 4 * 60, "2026-11-11"},
		{"first of the month, before cutoff", time.Date(2026, 12, 1, 1, 0, 0, 0, jakarta), jakarta, 4 * 60, "2026-11-30"},
		{"new year, before cutoff", time.Date(2027, 1, 1, 1, 0, 0, 0, jakarta), jakarta, 4 * 60, "2026-12-31"},
	} {
		if got := businessDate(tt.at, tt.loc, tt.cutoff); got != tt.want {
			t.Errorf("%s: got %s, want %s", tt.name, got, tt.want)
		}
	}
}

func TestFormatNumber(t *testing.T) {
	for _, tt := range []struct {
		prefix string
		seq    int
		want   string
	}{{"POS", 1, "POS-001"}, {"WEB", 42, "WEB-042"}, {"POS", 999, "POS-999"}, {"POS", 1000, "POS-1000"}} {
		if got := FormatNumber(tt.prefix, tt.seq); got != tt.want {
			t.Errorf("got %s, want %s", got, tt.want)
		}
	}
}

func TestCursorRoundTrip(t *testing.T) {
	c := cursor{date: "2026-11-12", openedAt: time.Date(2026, 11, 12, 2, 1, 0, 123456000, time.UTC),
		id: "0b0e7a52-5c0f-4d3a-9d0c-3a8b0f6f2a11"}
	got, ok := parseCursor(c.String())
	if !ok || got.date != c.date || !got.openedAt.Equal(c.openedAt) || got.id != c.id {
		t.Fatalf("round trip = %+v, %v", got, ok)
	}
	for _, bad := range []string{"", "!!", "bm90IGEgY3Vyc29y"} {
		if _, ok := parseCursor(bad); ok {
			t.Errorf("parseCursor(%q) accepted", bad)
		}
	}
}

func TestListFilter(t *testing.T) {
	for _, tt := range []struct {
		query string
		field string // the rejected field, "" when valid
	}{
		{"", ""},
		{"from=2026-08-01&to=2026-11-01", ""},   // 92 days
		{"from=2026-08-01&to=2026-11-02", "to"}, // 93 days
		{"from=2026-11-02&to=2026-11-01", "to"},
		{"from=2026-11-01", ""},
		{"from=11/01/2026", "from"},
		{"to=yesterday", "to"},
		{"status=open", ""},
		{"status=paused", "status"},
		{"limit=200", ""},
		{"limit=201", "limit"},
		{"limit=0", "limit"},
		{"cursor=nope", "cursor"},
	} {
		r := httptest.NewRequest("GET", "/?"+tt.query, nil)
		_, err := listFilter(r, "outlet")
		switch {
		case tt.field == "" && err != nil:
			t.Errorf("%q: unexpected error %v", tt.query, err)
		case tt.field != "" && err == nil:
			t.Errorf("%q: accepted", tt.query)
		}
	}
}
