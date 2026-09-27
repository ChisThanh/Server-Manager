package docker

import (
	"bufio"
	"encoding/json"
	"strconv"
	"strings"
	"time"
)

// parseSize converts docker's human sizes ("1.64MB", "1.109MiB", "572B",
// "12.3kB", "0B (virtual 1.6MB)") to bytes. Unknown → 0.
func parseSize(s string) int64 {
	s = strings.TrimSpace(s)
	if i := strings.IndexByte(s, ' '); i > 0 {
		s = s[:i]
	}
	if s == "" || s == "N/A" || s == "--" {
		return 0
	}
	i := 0
	for i < len(s) && (s[i] >= '0' && s[i] <= '9' || s[i] == '.') {
		i++
	}
	v, err := strconv.ParseFloat(s[:i], 64)
	if err != nil {
		return 0
	}
	mult := 1.0
	switch strings.ToLower(strings.TrimSpace(s[i:])) {
	case "", "b":
	case "kb", "k":
		mult = 1e3
	case "mb", "m":
		mult = 1e6
	case "gb", "g":
		mult = 1e9
	case "tb", "t":
		mult = 1e12
	case "pb":
		mult = 1e15
	case "kib":
		mult = 1 << 10
	case "mib":
		mult = 1 << 20
	case "gib":
		mult = 1 << 30
	case "tib":
		mult = 1 << 40
	case "pib":
		mult = 1 << 50
	default:
		return 0
	}
	return int64(v * mult)
}

// parsePair splits "a / b" sizes (stats MemUsage, NetIO, BlockIO).
func parsePair(s string) (int64, int64) {
	a, b, _ := strings.Cut(s, "/")
	return parseSize(a), parseSize(b)
}

func parsePct(s string) float64 {
	v, _ := strconv.ParseFloat(strings.TrimSuffix(strings.TrimSpace(s), "%"), 64)
	return v
}

// parseTime parses docker CLI timestamps ("2026-09-25 03:37:35 +0000 UTC",
// RFC3339 with nanoseconds). Returns unix seconds (0 when unknown).
func parseTime(s string) int64 {
	s = strings.TrimSpace(s)
	if s == "" || strings.HasPrefix(s, "0001-01-01") {
		return 0
	}
	for _, layout := range []string{"2006-01-02 15:04:05 -0700 MST", "2006-01-02 15:04:05.999999999 -0700 MST", time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, s); err == nil {
			return t.Unix()
		}
	}
	return 0
}

// healthOf extracts the health from a status like "Up 3 minutes (healthy)".
func healthOf(status string) string {
	switch {
	case strings.Contains(status, "(unhealthy)"):
		return "unhealthy"
	case strings.Contains(status, "(healthy)"):
		return "healthy"
	case strings.Contains(status, "(health: starting)"):
		return "starting"
	}
	return ""
}

// jsonLines decodes newline-delimited JSON objects, also accepting a single
// JSON array (older compose versions). Bad lines are skipped.
func jsonLines[T any](out string) []T {
	res := []T{}
	trim := strings.TrimSpace(out)
	if strings.HasPrefix(trim, "[") {
		var arr []T
		if json.Unmarshal([]byte(trim), &arr) == nil {
			return append(res, arr...)
		}
	}
	sc := bufio.NewScanner(strings.NewReader(out))
	sc.Buffer(make([]byte, 64*1024), 8<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if !strings.HasPrefix(line, "{") {
			continue
		}
		var v T
		if json.Unmarshal([]byte(line), &v) == nil {
			res = append(res, v)
		}
	}
	return res
}

// splitList splits a comma-separated label value.
func splitList(s string) []string {
	out := []string{}
	for _, p := range strings.Split(s, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}
