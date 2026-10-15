package main

import (
	"fmt"
	"strconv"

	"github.com/arugula-salad/loam/pkg/api"
)

func cmdStats(a *app, args []string) error {
	fs := a.flags("loam stats")
	days := fs.Int("days", 7, "days to show, today (UTC) last; at most 90")
	pos, err := parseArgs(fs, args, -1)
	if err != nil {
		return err
	}
	if len(pos) != 1 {
		return errUsage
	}
	c, err := a.client()
	if err != nil {
		return err
	}
	st, err := c.Stats(a.ctx, pos[0], *days)
	if err != nil {
		return err
	}
	return a.output(st, func() {
		row := func(label string, t api.Traffic, visitors string) []string {
			n := func(v int64) string { return strconv.FormatInt(v, 10) }
			return []string{label, visitors, n(t.Requests), n(t.Status2xx), n(t.Status3xx), n(t.Status4xx), n(t.Status5xx),
				n(t.WebSockets), size(t.BytesSent), n(t.KeyRequests)}
		}
		var rows [][]string
		for _, d := range st.Days {
			rows = append(rows, row(d.Day, d.Traffic, strconv.FormatInt(d.Visitors, 10)))
		}
		rows = append(rows, row("total", st.Total, strconv.FormatInt(st.VisitorDays, 10)+"*"))
		a.table([]string{"DAY", "VISITORS", "REQUESTS", "2XX", "3XX", "4XX", "5XX", "WS", "SENT", "KEY"}, rows)
		fmt.Fprintln(a.stdout, "* visitor-days: a visitor on two days counts twice")
	})
}

// size is a byte count for people: 512B, 3.2KiB, 48.8MiB.
func size(n int64) string {
	const unit = 1024
	if n < unit {
		return strconv.FormatInt(n, 10) + "B"
	}
	v, i := float64(n)/unit, 0
	for v >= unit && i < 4 {
		v /= unit
		i++
	}
	return fmt.Sprintf("%.1f%ciB", v, "KMGTP"[i])
}
