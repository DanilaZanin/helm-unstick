// Package humanize formats values for people.
package humanize

import (
	"fmt"
	"time"
)

// Duration renders d compactly: 45s, 12m5s, 2h13m, 3d4h. Negative values render as 0s.
func Duration(d time.Duration) string {
	if d < 0 {
		d = 0
	}
	total := int64(d.Round(time.Second) / time.Second)
	days, rem := total/86400, total%86400
	hours, rem := rem/3600, rem%3600
	mins, secs := rem/60, rem%60
	switch {
	case days > 0:
		if hours == 0 {
			return fmt.Sprintf("%dd", days)
		}
		return fmt.Sprintf("%dd%dh", days, hours)
	case hours > 0:
		if mins == 0 {
			return fmt.Sprintf("%dh", hours)
		}
		return fmt.Sprintf("%dh%dm", hours, mins)
	case mins > 0:
		if secs == 0 {
			return fmt.Sprintf("%dm", mins)
		}
		return fmt.Sprintf("%dm%ds", mins, secs)
	default:
		return fmt.Sprintf("%ds", secs)
	}
}
