package expr

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ParseISODuration parses an ISO-8601 duration like "P1DT2H30M", "PT45S" or
// "P2W" into a time.Duration. Years and months use the civil approximations
// 365 and 30 days, matching common BPM engine behavior.
func ParseISODuration(s string) (time.Duration, error) {
	orig := s
	s = strings.TrimSpace(s)
	neg := false
	if strings.HasPrefix(s, "-") {
		neg = true
		s = s[1:]
	}
	if len(s) < 2 || (s[0] != 'P' && s[0] != 'p') {
		return 0, fmt.Errorf("expr: invalid ISO-8601 duration %q", orig)
	}
	s = s[1:]

	var total time.Duration
	inTime := false
	num := ""
	consume := func(unit byte) error {
		if num == "" {
			return fmt.Errorf("expr: invalid ISO-8601 duration %q: missing number before %q", orig, string(unit))
		}
		f, err := strconv.ParseFloat(num, 64)
		if err != nil {
			return fmt.Errorf("expr: invalid number %q in duration %q", num, orig)
		}
		num = ""
		var unitDur time.Duration
		switch {
		case !inTime && (unit == 'Y' || unit == 'y'):
			unitDur = 365 * 24 * time.Hour
		case !inTime && (unit == 'M'):
			unitDur = 30 * 24 * time.Hour
		case !inTime && (unit == 'W' || unit == 'w'):
			unitDur = 7 * 24 * time.Hour
		case !inTime && (unit == 'D' || unit == 'd'):
			unitDur = 24 * time.Hour
		case inTime && (unit == 'H' || unit == 'h'):
			unitDur = time.Hour
		case inTime && (unit == 'M' || unit == 'm'):
			unitDur = time.Minute
		case inTime && (unit == 'S' || unit == 's'):
			unitDur = time.Second
		default:
			return fmt.Errorf("expr: unexpected unit %q in duration %q", string(unit), orig)
		}
		total += time.Duration(f * float64(unitDur))
		return nil
	}

	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c == 'T' || c == 't':
			if inTime {
				return 0, fmt.Errorf("expr: invalid ISO-8601 duration %q", orig)
			}
			inTime = true
		case (c >= '0' && c <= '9') || c == '.':
			num += string(c)
		default:
			if err := consume(c); err != nil {
				return 0, err
			}
		}
	}
	if num != "" {
		return 0, fmt.Errorf("expr: trailing number in duration %q", orig)
	}
	if total == 0 && !strings.ContainsAny(s, "0") {
		// "P" or "PT" alone is invalid.
		if !strings.ContainsAny(s, "123456789") {
			return 0, fmt.Errorf("expr: empty ISO-8601 duration %q", orig)
		}
	}
	if neg {
		total = -total
	}
	return total, nil
}

// ParseTimerCycle parses an ISO-8601 repeating interval "R<n>/<duration>"
// (e.g. "R3/PT10S", "R/PT1H" for unbounded) and returns the repetition
// count (-1 = unbounded) and the interval.
func ParseTimerCycle(s string) (repeats int, interval time.Duration, err error) {
	s = strings.TrimSpace(s)
	if !strings.HasPrefix(s, "R") && !strings.HasPrefix(s, "r") {
		return 0, 0, fmt.Errorf("expr: invalid timer cycle %q", s)
	}
	parts := strings.SplitN(s[1:], "/", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("expr: invalid timer cycle %q (want R<n>/<duration>)", s)
	}
	repeats = -1
	if parts[0] != "" {
		n, convErr := strconv.Atoi(parts[0])
		if convErr != nil || n < 0 {
			return 0, 0, fmt.Errorf("expr: invalid repeat count in %q", s)
		}
		repeats = n
	}
	interval, err = ParseISODuration(parts[1])
	if err != nil {
		return 0, 0, err
	}
	return repeats, interval, nil
}
