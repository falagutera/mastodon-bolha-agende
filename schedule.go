package main

import (
	"errors"
	"fmt"
	"regexp"
	"time"
)

const (
	// Mastodon refuses less than 5 minutes; the margin covers the request itself.
	minLead      = 6 * time.Minute
	maxSeries    = 24
	dailyLimit   = 25
	totalLimit   = 300
	dateTimeForm = "2006-01-02T15:04"
)

var (
	errTooSoon     = errors.New("too_soon")
	errDailyLimit  = errors.New("daily_limit")
	errTotalLimit  = errors.New("total_limit")
	errBadDate     = errors.New("bad_date")
	errBadSeries   = errors.New("bad_series")
	errBadReply    = errors.New("bad_reply")
	idPattern      = regexp.MustCompile(`^[0-9]{1,24}$`)
	replyURLSuffix = regexp.MustCompile(`/([0-9]{1,24})/?$`)
)

// parseLocal reads a datetime-local value as a moment in the given zone.
func parseLocal(value string, loc *time.Location) (time.Time, error) {
	t, err := time.ParseInLocation(dateTimeForm, value, loc)
	if err != nil {
		return time.Time{}, errBadDate
	}
	return t, nil
}

// series expands a first date into its occurrences. A monthly series keeps the
// chosen day and falls back to the last day of shorter months.
func series(first time.Time, frequency string, count int) ([]time.Time, error) {
	if frequency == "" || frequency == "once" {
		return []time.Time{first}, nil
	}
	if count < 2 || count > maxSeries {
		return nil, errBadSeries
	}
	out := make([]time.Time, 0, count)
	for i := range count {
		switch frequency {
		case "weekly":
			out = append(out, first.AddDate(0, 0, 7*i))
		case "monthly":
			out = append(out, addMonths(first, i))
		default:
			return nil, errBadSeries
		}
	}
	return out, nil
}

func addMonths(t time.Time, n int) time.Time {
	y, m, d := t.Date()
	firstOfMonth := time.Date(y, m+time.Month(n), 1, t.Hour(), t.Minute(), 0, 0, t.Location())
	if last := firstOfMonth.AddDate(0, 1, -1).Day(); d > last {
		d = last
	}
	return time.Date(firstOfMonth.Year(), firstOfMonth.Month(), d, t.Hour(), t.Minute(), 0, 0, t.Location())
}

// checkDates applies Mastodon's rules before anything is sent, so a series
// doesn't stop halfway. Daily limits count UTC days, as Mastodon does.
func checkDates(dates []time.Time, existing []scheduledStatus, now time.Time) error {
	if len(existing)+len(dates) > totalLimit {
		return errTotalLimit
	}
	perDay := map[string]int{}
	for _, s := range existing {
		perDay[s.ScheduledAt.UTC().Format(time.DateOnly)]++
	}
	for _, d := range dates {
		if d.Before(now.Add(minLead)) {
			return errTooSoon
		}
		day := d.UTC().Format(time.DateOnly)
		perDay[day]++
		if perDay[day] > dailyLimit {
			return errDailyLimit
		}
	}
	return nil
}

// replyID accepts a post id or a URL of a post on this instance.
func replyID(value, instance string) (string, error) {
	if value == "" || idPattern.MatchString(value) {
		return value, nil
	}
	if len(value) > len(instance) && value[:len(instance)+1] == instance+"/" {
		if m := replyURLSuffix.FindStringSubmatch(value); m != nil {
			return m[1], nil
		}
	}
	return "", fmt.Errorf("%w", errBadReply)
}
