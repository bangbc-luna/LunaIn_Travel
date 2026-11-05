package reconcile

import (
	"time"

	"github.com/riverqueue/river"
)

// DailyAt is a River schedule firing every day at hour:minute in loc.
func DailyAt(hour, minute int, loc *time.Location) river.PeriodicSchedule {
	return dailyAt{hour: hour, minute: minute, loc: loc}
}

type dailyAt struct {
	hour, minute int
	loc          *time.Location
}

// Next is the first hour:minute in loc strictly after t.
func (d dailyAt) Next(t time.Time) time.Time {
	lt := t.In(d.loc)
	next := time.Date(lt.Year(), lt.Month(), lt.Day(), d.hour, d.minute, 0, 0, d.loc)
	if !next.After(t) {
		next = time.Date(lt.Year(), lt.Month(), lt.Day()+1, d.hour, d.minute, 0, 0, d.loc)
	}
	return next
}
