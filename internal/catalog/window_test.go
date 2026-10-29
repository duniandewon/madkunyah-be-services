package catalog

import (
	"testing"
	"time"
)

// AC-11, E-12, E-13, E-15.
func TestInWindow(t *testing.T) {
	jakarta, err := location("Asia/Jakarta")
	if err != nil {
		t.Fatal(err)
	}
	makassar, err := location("Asia/Makassar")
	if err != nil {
		t.Fatal(err)
	}
	morning := &window{from: 7 * 60, to: 11 * 60}
	night := &window{from: 22 * 60, to: 2 * 60}
	at := func(loc *time.Location, h, m int) time.Time {
		return time.Date(2026, 10, 29, h, m, 0, 0, loc)
	}

	tests := []struct {
		name string
		w    *window
		at   time.Time
		loc  *time.Location
		want bool
	}{
		{"no window", nil, at(jakarta, 3, 0), jakarta, true},
		{"opens at from", morning, at(jakarta, 7, 0), jakarta, true},
		{"before close", morning, at(jakarta, 10, 59), jakarta, true},
		{"to is exclusive", morning, at(jakarta, 11, 0), jakarta, false},
		{"before open", morning, at(jakarta, 6, 59), jakarta, false},
		{"wrap evening", night, at(jakarta, 23, 0), jakarta, true},
		{"wrap after midnight", night, at(jakarta, 1, 30), jakarta, true},
		{"wrap before close", night, at(jakarta, 1, 59), jakarta, true},
		{"wrap close exclusive", night, at(jakarta, 2, 0), jakarta, false},
		{"wrap midday", night, at(jakarta, 12, 0), jakarta, false},
		// 10:30 in Jakarta is 11:30 in Makassar: the same instant is in
		// window at one outlet and out at the other.
		{"jakarta local", morning, at(jakarta, 10, 30), jakarta, true},
		{"makassar local", morning, at(jakarta, 10, 30), makassar, false},
		{"utc instant", morning, time.Date(2026, 10, 29, 0, 0, 0, 0, time.UTC), jakarta, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := inWindow(tt.w, tt.at, tt.loc); got != tt.want {
				t.Errorf("inWindow = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestParseClock(t *testing.T) {
	for in, want := range map[string]bool{
		"00:00": true, "23:59": true, "07:30": true,
		"24:00": false, "7:30": false, "07:60": false, "07-30": false, "": false, "0a:00": false,
	} {
		if _, ok := parseClock(in); ok != want {
			t.Errorf("parseClock(%q) ok = %v, want %v", in, ok, want)
		}
	}
}
