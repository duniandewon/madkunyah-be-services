package shift

import "time"

const dateLayout = "2006-01-02"

// businessDate is the trading day a shift opened at `at` belongs to: the
// outlet-local date, or the day before when the outlet's cutoff has not
// passed yet (FR-SH-6). cutoff is minutes after local midnight.
func businessDate(at time.Time, loc *time.Location, cutoff int) string {
	local := at.In(loc)
	day := local.Day()
	if local.Hour()*60+local.Minute() < cutoff {
		day--
	}
	return time.Date(local.Year(), local.Month(), day, 0, 0, 0, 0, time.UTC).Format(dateLayout)
}
