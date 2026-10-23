package engine

import (
	"fmt"
	"time"
)

func roundDur(d time.Duration) time.Duration {
	if d >= time.Minute {
		return d.Round(time.Second)
	}
	return d.Round(100 * time.Millisecond)
}

func humanBytes(n int64) string {
	const unit = 1024
	if n < unit {
		return fmt.Sprintf("%d B", n)
	}
	div, exp := int64(unit), 0
	for m := n / unit; m >= unit; m /= unit {
		div *= unit
		exp++
	}
	return fmt.Sprintf("%.1f %cB", float64(n)/float64(div), "KMGTPE"[exp])
}

func rateWords(n int64, d time.Duration) string {
	if d <= 0 || n <= 0 {
		return "—"
	}
	return humanBytes(int64(float64(n)/d.Seconds())) + "/s"
}
