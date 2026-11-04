// Package retryafter reads the delay a provider asks for before the next request.
package retryafter

import (
	"math"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// Max bounds a parsed delay: a hint longer than a day is a misconfigured server, and the loop treats
// any hint beyond the run's deadline or outage window as "do not wait" anyway.
const Max = 24 * time.Hour

// Parse returns the delay h asks for, 0 when it asks for none. retry-after-ms (milliseconds, the
// OpenAI and Anthropic SDK extension) takes precedence over Retry-After, which is a number of seconds
// (fractions accepted) or an HTTP date. Negative, unparsable and past values are 0; a value above Max
// is Max.
func Parse(h http.Header, now time.Time) time.Duration {
	if ms, ok := number(h.Get("retry-after-ms")); ok {
		return seconds(ms / 1000)
	}
	value := strings.TrimSpace(h.Get("Retry-After"))
	if value == "" {
		return 0
	}
	if s, ok := number(value); ok {
		return seconds(s)
	}
	if at, err := http.ParseTime(value); err == nil {
		if d := at.Sub(now); d > 0 {
			return min(d, Max)
		}
	}
	return 0
}

// number parses a non-negative decimal; anything else (a date, NaN, a sign) is not a number of units.
func number(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(value, 64)
	if err != nil || math.IsNaN(n) || n < 0 {
		return 0, false
	}
	return n, true
}

// seconds converts s to a duration bounded by Max, before the conversion can overflow.
func seconds(s float64) time.Duration {
	if s >= Max.Seconds() {
		return Max
	}
	return time.Duration(s * float64(time.Second))
}
