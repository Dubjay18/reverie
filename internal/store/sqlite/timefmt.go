package sqlite

import "time"

// Fixed-width UTC so lexicographic order equals chronological order.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

func fmtTime(t time.Time) string { return t.UTC().Format(timeLayout) }

func parseTime(s string) (time.Time, error) { return time.ParseInLocation(timeLayout, s, time.UTC) }
