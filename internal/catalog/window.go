package catalog

import (
	"sync"
	"time"
)

// inWindow evaluates a daily [from, to) window, in minutes after midnight,
// at the wall-clock time of at in loc (FR-AVL-1..5). from > to wraps past
// midnight. A nil window is always open.
func inWindow(w *window, at time.Time, loc *time.Location) bool {
	if w == nil {
		return true
	}
	local := at.In(loc)
	m := local.Hour()*60 + local.Minute()
	if w.from < w.to {
		return m >= w.from && m < w.to
	}
	return m >= w.from || m < w.to
}

type window struct {
	from, to int
}

// newWindow converts stored "HH:MM" ends into a window, or nil when unset.
func newWindow(from, to *string) *window {
	if from == nil || to == nil {
		return nil
	}
	f, okF := parseClock(*from)
	t, okT := parseClock(*to)
	if !okF || !okT {
		return nil
	}
	return &window{from: f, to: t}
}

var locations sync.Map

// location caches time.LoadLocation, which reads the zone database from
// disk on every call.
func location(name string) (*time.Location, error) {
	if l, ok := locations.Load(name); ok {
		return l.(*time.Location), nil
	}
	l, err := time.LoadLocation(name)
	if err != nil {
		return nil, err
	}
	locations.Store(name, l)
	return l, nil
}
