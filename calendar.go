package main

import (
	"sort"
	"time"
)

type calendarItem struct {
	ID      string
	Time    string
	Excerpt string
}

type calendarDay struct {
	Date    time.Time
	Label   string
	Today   bool
	Outside bool
	Items   []calendarItem
}

type calendar struct {
	Weeks [][]calendarDay
}

// weekStart returns the Sunday of the week holding t.
func weekStart(t time.Time) time.Time {
	y, m, d := t.Date()
	day := time.Date(y, m, d, 0, 0, 0, 0, t.Location())
	return day.AddDate(0, 0, -int(day.Weekday()))
}

// buildCalendar lays out a week or the weeks touching the month of anchor.
func buildCalendar(view string, anchor, now time.Time, posts []scheduledStatus) calendar {
	loc := anchor.Location()
	start := weekStart(anchor)
	weeks := 1
	if view == "month" {
		y, m, _ := anchor.Date()
		first := time.Date(y, m, 1, 0, 0, 0, 0, loc)
		start = weekStart(first)
		last := first.AddDate(0, 1, -1)
		weeks = int(weekStart(last).Sub(start).Hours()/24/7+0.5) + 1
	}
	byDay := map[string][]scheduledStatus{}
	for _, p := range posts {
		key := p.ScheduledAt.In(loc).Format(time.DateOnly)
		byDay[key] = append(byDay[key], p)
	}
	var c calendar
	for w := range weeks {
		var row []calendarDay
		for d := range 7 {
			day := start.AddDate(0, 0, w*7+d)
			key := day.Format(time.DateOnly)
			entry := calendarDay{
				Date:    day,
				Label:   day.Format("2"),
				Today:   key == now.In(loc).Format(time.DateOnly),
				Outside: view == "month" && day.Month() != anchor.Month(),
			}
			posts := byDay[key]
			sort.Slice(posts, func(i, j int) bool { return posts[i].ScheduledAt.Before(posts[j].ScheduledAt) })
			for _, p := range posts {
				entry.Items = append(entry.Items, calendarItem{ID: p.ID, Time: p.ScheduledAt.In(loc).Format("15:04"), Excerpt: excerpt(p.Params.Text)})
			}
			row = append(row, entry)
		}
		c.Weeks = append(c.Weeks, row)
	}
	return c
}
