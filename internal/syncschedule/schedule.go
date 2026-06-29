package syncschedule

import (
	"fmt"
	"strings"
	"time"
)

func Parse(raw string) (time.Duration, bool, error) {
	value := strings.ToLower(strings.TrimSpace(raw))
	if value == "" || value == "manual" {
		return 0, false, nil
	}
	switch value {
	case "hourly":
		return time.Hour, true, nil
	case "daily":
		return 24 * time.Hour, true, nil
	case "weekly":
		return 7 * 24 * time.Hour, true, nil
	}
	if strings.HasPrefix(value, "every ") {
		duration, err := time.ParseDuration(strings.TrimSpace(strings.TrimPrefix(value, "every ")))
		if err != nil || duration <= 0 {
			return 0, false, fmt.Errorf("invalid sync_schedule %q", raw)
		}
		return duration, true, nil
	}
	return 0, false, fmt.Errorf("invalid sync_schedule %q", raw)
}
