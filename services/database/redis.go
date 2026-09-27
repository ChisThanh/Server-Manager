package database

import (
	"context"
	"regexp"
	"strconv"
	"strings"
	"time"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// Redis: commands are sent to redis-cli on stdin (never through a shell),
// one per line, each argument re-encoded as a double-quoted string with
// \xHH escapes so any byte survives. Replies use redis-cli's --no-raw
// (TTY) format; an ECHO marker after each command delimits the replies.

func redisArgs(t Target, db int) []string {
	args := []string{"--no-raw"}
	switch {
	case strings.HasPrefix(t.Host, "/"):
		args = append(args, "-s", t.Host)
	case t.Host != "":
		args = append(args, "-h", t.Host)
	}
	if t.Port > 0 {
		args = append(args, "-p", strconv.Itoa(t.Port))
	}
	if t.Auth == AuthPassword && t.User != "" {
		args = append(args, "--user", t.User)
	}
	if db > 0 {
		args = append(args, "-n", strconv.Itoa(db))
	}
	return args
}

// encodeRedisArg quotes an argument for redis-cli's line parser.
func encodeRedisArg(a string) string {
	var b strings.Builder
	b.WriteByte('"')
	for i := 0; i < len(a); i++ {
		c := a[i]
		switch {
		case c == '\\':
			b.WriteString(`\\`)
		case c == '"':
			b.WriteString(`\"`)
		case c >= 0x20 && c < 0x7f:
			b.WriteByte(c)
		default:
			b.WriteString(`\x`)
			b.WriteString(strconv.FormatUint(uint64(c)>>4, 16))
			b.WriteString(strconv.FormatUint(uint64(c)&15, 16))
		}
	}
	b.WriteByte('"')
	return b.String()
}

func encodeRedisLine(args []string) string {
	q := make([]string, len(args))
	for i, a := range args {
		q[i] = encodeRedisArg(a)
	}
	return strings.Join(q, " ")
}

// splitRedisCommand splits a command typed by the user like redis-cli does:
// whitespace-separated words, "double quoted" (with \n \r \t \b \a \" \\
// \xHH escapes) or 'single quoted' (with \' escape) arguments.
func splitRedisCommand(line string) ([]string, error) {
	var args []string
	i, n := 0, len(line)
	for {
		for i < n && (line[i] == ' ' || line[i] == '\t' || line[i] == '\n' || line[i] == '\r') {
			i++
		}
		if i >= n {
			return args, nil
		}
		var cur strings.Builder
		inDq, inSq, done := false, false, false
		for !done {
			if i >= n {
				if inDq || inSq {
					return nil, apperr.New("db.redis.badQuote")
				}
				break
			}
			c := line[i]
			switch {
			case inDq:
				if c == '\\' && i+3 < n && line[i+1] == 'x' && isHex(line[i+2]) && isHex(line[i+3]) {
					v, _ := strconv.ParseUint(line[i+2:i+4], 16, 8)
					cur.WriteByte(byte(v))
					i += 3
				} else if c == '\\' && i+1 < n {
					i++
					switch line[i] {
					case 'n':
						cur.WriteByte('\n')
					case 'r':
						cur.WriteByte('\r')
					case 't':
						cur.WriteByte('\t')
					case 'b':
						cur.WriteByte('\b')
					case 'a':
						cur.WriteByte('\a')
					default:
						cur.WriteByte(line[i])
					}
				} else if c == '"' {
					if i+1 < n && !isSpace(line[i+1]) {
						return nil, apperr.New("db.redis.badQuote")
					}
					done = true
				} else {
					cur.WriteByte(c)
				}
			case inSq:
				if c == '\\' && i+1 < n && line[i+1] == '\'' {
					i++
					cur.WriteByte('\'')
				} else if c == '\'' {
					if i+1 < n && !isSpace(line[i+1]) {
						return nil, apperr.New("db.redis.badQuote")
					}
					done = true
				} else {
					cur.WriteByte(c)
				}
			default:
				switch c {
				case ' ', '\t', '\n', '\r':
					done = true
				case '"':
					inDq = true
				case '\'':
					inSq = true
				default:
					cur.WriteByte(c)
				}
			}
			i++
		}
		args = append(args, cur.String())
	}
}

func isHex(c byte) bool {
	return (c >= '0' && c <= '9') || (c >= 'a' && c <= 'f') || (c >= 'A' && c <= 'F')
}

func isSpace(c byte) bool { return c == ' ' || c == '\t' || c == '\n' || c == '\r' }

// runRedis sends commands (argument lists) in one redis-cli session and
// returns each reply in redis-cli's TTY format.
func (s *DatabaseService) runRedis(ctx context.Context, conn *sshx.Conn, t Target, db int, cmds [][]string, sudoPW string) ([]string, error) {
	mark := markerPrefix + randomHex(8)
	var in strings.Builder
	for _, c := range cmds {
		in.WriteString(encodeRedisLine(c) + "\n")
		in.WriteString("ECHO " + mark + "\n")
	}
	res, err := s.runClient(ctx, conn, t, redisArgs(t, db), nil, in.String(), 4<<20, sudoPW)
	if err != nil {
		return nil, err
	}
	out := strings.ReplaceAll(res.Stdout, "\r\n", "\n")
	if strings.Contains(out, "NOAUTH") || strings.Contains(out, "WRONGPASS") || strings.Contains(out, "invalid password") {
		if !strings.Contains(out, `"`+mark+`"`) {
			return nil, apperr.New("db.redis.authRequired").WithDetail(firstLines(out, 2))
		}
	}
	quoted := `"` + mark + `"`
	parts := []string{}
	rest := out
	for len(parts) < len(cmds) {
		i := strings.Index(rest, quoted+"\n")
		if i < 0 {
			if j := strings.Index(rest, quoted); j >= 0 && j+len(quoted) == len(strings.TrimRight(rest, "\n")) {
				i = j
			} else {
				break
			}
		}
		if i > 0 && rest[i-1] != '\n' {
			// the marker must start a line
			break
		}
		parts = append(parts, strings.TrimSuffix(rest[:i], "\n"))
		rest = rest[min(len(rest), i+len(quoted)+1):]
	}
	if len(parts) < len(cmds) {
		msg := core.FirstNonEmpty(strings.TrimSpace(res.Stderr), strings.TrimSpace(rest))
		if reConnFail.MatchString(msg) {
			return nil, apperr.New("db.connectFailed").WithDetail(firstLines(msg, 3))
		}
		return nil, apperr.New("db.redis.failed").WithDetail(firstLines(msg, 5))
	}
	return parts, nil
}

// ---- TTY reply parsing ----

// rnode is a parsed redis-cli TTY reply.
type rnode struct {
	kind string // str, int, nil, err, status, arr, double, bool
	s    string
	arr  []rnode
}

var reTTYIndex = regexp.MustCompile(`^( *)(\d+)\) `)

// parseTTY parses redis-cli's --no-raw output of one reply.
func parseTTY(text string) rnode {
	lines := strings.Split(strings.TrimRight(text, "\n"), "\n")
	p := &ttyParser{lines: lines}
	if len(lines) == 0 {
		return rnode{kind: "nil"}
	}
	return p.value(lines[0], 0)
}

type ttyParser struct {
	lines []string
	pos   int // index of the current line
}

// value parses a value whose first line text is `first` (already stripped
// of its prefixes); continuation lines are indented by `indent` spaces.
func (p *ttyParser) value(first string, indent int) rnode {
	if m := reTTYIndex.FindStringSubmatch(first); m != nil && m[2] == "1" {
		// array: elements "N) value", continuation at indent+len(prefix)
		arr := []rnode{}
		expect := 1
		text := first
		for {
			m := reTTYIndex.FindStringSubmatch(text)
			if m == nil || m[2] != strconv.Itoa(expect) {
				break
			}
			plen := len(m[0])
			arr = append(arr, p.value(text[plen:], indent+plen))
			expect++
			// next element: a following line with exactly `indent` spaces
			// (plus index padding) and the next index
			if p.pos+1 >= len(p.lines) {
				break
			}
			next := p.lines[p.pos+1]
			if len(next) < indent || strings.TrimLeft(next[:indent], " ") != "" {
				break
			}
			cand := next[indent:]
			m2 := reTTYIndex.FindStringSubmatch(cand)
			if m2 == nil || m2[2] != strconv.Itoa(expect) {
				break
			}
			p.pos++
			text = cand
		}
		return rnode{kind: "arr", arr: arr}
	}
	return scalarTTY(first)
}

func scalarTTY(t string) rnode {
	switch {
	case t == "(nil)":
		return rnode{kind: "nil"}
	case t == "(empty array)" || t == "(empty list or set)" || t == "(empty set)":
		return rnode{kind: "arr", arr: []rnode{}}
	case strings.HasPrefix(t, "(integer) "):
		return rnode{kind: "int", s: strings.TrimPrefix(t, "(integer) ")}
	case strings.HasPrefix(t, "(double) "):
		return rnode{kind: "double", s: strings.TrimPrefix(t, "(double) ")}
	case strings.HasPrefix(t, "(error) "):
		return rnode{kind: "err", s: strings.TrimPrefix(t, "(error) ")}
	case t == "(true)" || t == "(false)":
		return rnode{kind: "bool", s: strings.Trim(t, "()")}
	case strings.HasPrefix(t, `"`):
		return rnode{kind: "str", s: unrepr(t)}
	}
	return rnode{kind: "status", s: t}
}

// unrepr decodes a string printed by redis' sdscatrepr ("…" with escapes).
func unrepr(t string) string {
	if len(t) < 2 || t[0] != '"' || t[len(t)-1] != '"' {
		return t
	}
	t = t[1 : len(t)-1]
	var b strings.Builder
	for i := 0; i < len(t); i++ {
		c := t[i]
		if c != '\\' || i+1 >= len(t) {
			b.WriteByte(c)
			continue
		}
		i++
		switch t[i] {
		case 'n':
			b.WriteByte('\n')
		case 'r':
			b.WriteByte('\r')
		case 't':
			b.WriteByte('\t')
		case 'a':
			b.WriteByte('\a')
		case 'b':
			b.WriteByte('\b')
		case 'x':
			if i+2 < len(t) && isHex(t[i+1]) && isHex(t[i+2]) {
				v, _ := strconv.ParseUint(t[i+1:i+3], 16, 8)
				b.WriteByte(byte(v))
				i += 2
			} else {
				b.WriteString(`\x`)
			}
		default:
			b.WriteByte(t[i])
		}
	}
	return b.String()
}

// ---- INFO ----

func parseInfo(text string) []InfoSection {
	text = strings.ReplaceAll(text, "\r\n", "\n")
	if strings.HasPrefix(text, `"`) {
		text = unrepr(strings.TrimSpace(text))
		text = strings.ReplaceAll(text, "\r\n", "\n")
	}
	out := []InfoSection{}
	cur := -1
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if strings.HasPrefix(line, "# ") {
			out = append(out, InfoSection{Name: strings.TrimPrefix(line, "# "), Items: []KV{}})
			cur = len(out) - 1
			continue
		}
		k, v, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		if cur < 0 {
			out = append(out, InfoSection{Name: "", Items: []KV{}})
			cur = 0
		}
		out[cur].Items = append(out[cur].Items, KV{Name: k, Value: v})
	}
	return out
}

func infoMap(secs []InfoSection) map[string]string {
	m := map[string]string{}
	for _, s := range secs {
		for _, kv := range s.Items {
			m[kv.Name] = kv.Value
		}
	}
	return m
}

func subFields(v string) map[string]string {
	m := map[string]string{}
	for _, p := range strings.Split(v, ",") {
		if k, val, ok := strings.Cut(p, "="); ok {
			m[k] = val
		}
	}
	return m
}

func redisOverviewFrom(info string, slow, policy, maxmem rnode) (Overview, error) {
	secs := parseInfo(info)
	m := infoMap(secs)
	if len(m) == 0 {
		return Overview{}, apperr.New("db.redis.failed").WithDetail(firstLines(info, 3))
	}
	f := func(k string) float64 { v, _ := strconv.ParseFloat(m[k], 64); return v }
	i := func(k string) int64 { return atoi64(m[k]) }
	r := &RedisOverview{Mode: m["redis_mode"], Role: m["role"], ConnectedClients: i("connected_clients"), BlockedClients: i("blocked_clients"),
		MaxClients: i("maxclients"), UsedMemory: i("used_memory"), UsedMemoryPeak: i("used_memory_peak"), UsedMemoryRSS: i("used_memory_rss"),
		MaxMemory: i("maxmemory"), MaxMemoryPolicy: m["maxmemory_policy"], FragRatio: f("mem_fragmentation_ratio"),
		EvictedKeys: i("evicted_keys"), ExpiredKeys: i("expired_keys"), OpsPerSec: f("instantaneous_ops_per_sec"),
		TotalCommands: i("total_commands_processed"), Hits: i("keyspace_hits"), Misses: i("keyspace_misses"), HitRatio: -1,
		RDBLastSave: i("rdb_last_save_time"), RDBLastStatus: m["rdb_last_bgsave_status"], RDBChanges: i("rdb_changes_since_last_save"),
		RDBSaving: m["rdb_bgsave_in_progress"] == "1", AOFEnabled: m["aof_enabled"] == "1", AOFLastStatus: m["aof_last_write_status"],
		AOFRewriting: m["aof_rewrite_in_progress"] == "1", ConnectedSlaves: i("connected_slaves"), MasterHost: m["master_host"],
		MasterLinkStatus: m["master_link_status"], Replicas: []string{}, Keyspace: []RedisDB{}, Slowlog: []RedisSlow{}, Sections: secs}
	if r.Hits+r.Misses > 0 {
		r.HitRatio = float64(r.Hits) / float64(r.Hits+r.Misses)
	}
	if m["master_host"] != "" {
		r.MasterHost = m["master_host"] + ":" + m["master_port"]
	}
	for n := 0; n < 64; n++ {
		v, ok := m["slave"+strconv.Itoa(n)]
		if !ok {
			break
		}
		sf := subFields(v)
		r.Replicas = append(r.Replicas, sf["ip"]+":"+sf["port"]+" "+sf["state"]+" lag="+sf["lag"])
	}
	for _, s := range secs {
		if !strings.EqualFold(s.Name, "Keyspace") {
			continue
		}
		for _, kv := range s.Items {
			n, err := strconv.Atoi(strings.TrimPrefix(kv.Name, "db"))
			if err != nil {
				continue
			}
			sf := subFields(kv.Value)
			r.Keyspace = append(r.Keyspace, RedisDB{DB: n, Keys: atoi64(sf["keys"]), Expires: atoi64(sf["expires"]), AvgTTL: atoi64(sf["avg_ttl"])})
		}
	}
	// CONFIG GET replies ([name, value]); CONFIG may be disabled or renamed.
	if policy.kind == "err" {
		r.ConfigError = policy.s
	} else if policy.kind == "arr" && len(policy.arr) == 2 {
		r.MaxMemoryPolicy = policy.arr[1].s
	}
	if maxmem.kind == "arr" && len(maxmem.arr) == 2 && r.MaxMemory == 0 {
		r.MaxMemory = atoi64(maxmem.arr[1].s)
	}
	if slow.kind == "err" {
		r.SlowlogError = slow.s
	}
	for _, e := range slow.arr {
		if e.kind != "arr" || len(e.arr) < 4 {
			continue
		}
		it := RedisSlow{ID: atoi64(e.arr[0].s), TS: atoi64(e.arr[1].s), Duration: atoi64(e.arr[2].s)}
		var words []string
		for _, a := range e.arr[3].arr {
			words = append(words, redisDisplayArg(a.s))
		}
		it.Command = shorten(strings.Join(words, " "), 500)
		if len(e.arr) > 4 {
			it.Client = e.arr[4].s
		}
		if len(e.arr) > 5 {
			it.Name = e.arr[5].s
		}
		r.Slowlog = append(r.Slowlog, it)
	}
	o := Overview{Engine: EngineRedis, Version: m["redis_version"], Uptime: i("uptime_in_seconds"), Redis: r}
	if v := m["valkey_version"]; v != "" {
		o.Version = v
	}
	return o, nil
}

func redisDisplayArg(a string) string {
	if a == "" {
		return `""`
	}
	for i := 0; i < len(a); i++ {
		if a[i] <= ' ' || a[i] >= 0x7f || a[i] == '"' {
			return encodeRedisArg(a)
		}
	}
	return a
}

func (s *DatabaseService) redisOverview(ctx context.Context, conn *sshx.Conn, t Target, sudoPW string) (Overview, error) {
	parts, err := s.runRedis(ctx, conn, t, 0, [][]string{
		{"INFO", "everything"}, {"SLOWLOG", "GET", "25"}, {"CONFIG", "GET", "maxmemory-policy"}, {"CONFIG", "GET", "maxmemory"},
	}, sudoPW)
	if err != nil {
		return Overview{}, err
	}
	info := parts[0]
	if strings.HasPrefix(info, "(error)") {
		// INFO everything needs Redis 7; plain INFO works everywhere
		p2, err := s.runRedis(ctx, conn, t, 0, [][]string{{"INFO"}}, sudoPW)
		if err != nil {
			return Overview{}, err
		}
		info = p2[0]
	}
	if strings.HasPrefix(info, "(error)") {
		return Overview{}, apperr.New("db.redis.failed").WithDetail(strings.TrimPrefix(info, "(error) "))
	}
	return redisOverviewFrom(info, parseTTY(parts[1]), parseTTY(parts[2]), parseTTY(parts[3]))
}

// ---- command console ----

// Blocked in the console: streaming/connection-state commands that make no
// sense in a one-shot session, and authentication (use the target's
// password instead).
var redisBlocked = map[string]bool{
	"MONITOR": true, "SUBSCRIBE": true, "PSUBSCRIBE": true, "SSUBSCRIBE": true, "SYNC": true, "PSYNC": true,
	"AUTH": true, "HELLO": true, "SELECT": true, "MULTI": true, "EXEC": true, "DISCARD": true, "WATCH": true,
	"UNWATCH": true, "QUIT": true, "RESET": true, "REPLCONF": true,
}

var redisRead = map[string]bool{
	"GET": true, "MGET": true, "EXISTS": true, "TYPE": true, "TTL": true, "PTTL": true, "EXPIRETIME": true, "PEXPIRETIME": true,
	"STRLEN": true, "GETRANGE": true, "SUBSTR": true, "LCS": true, "HGET": true, "HGETALL": true, "HKEYS": true, "HVALS": true,
	"HLEN": true, "HMGET": true, "HEXISTS": true, "HSTRLEN": true, "HSCAN": true, "HRANDFIELD": true, "LRANGE": true, "LLEN": true,
	"LINDEX": true, "LPOS": true, "SMEMBERS": true, "SCARD": true, "SISMEMBER": true, "SMISMEMBER": true, "SRANDMEMBER": true,
	"SSCAN": true, "SINTER": true, "SUNION": true, "SDIFF": true, "SINTERCARD": true, "ZRANGE": true, "ZRANGEBYSCORE": true,
	"ZREVRANGE": true, "ZREVRANGEBYSCORE": true, "ZRANGEBYLEX": true, "ZREVRANGEBYLEX": true, "ZCARD": true, "ZSCORE": true,
	"ZMSCORE": true, "ZRANK": true, "ZREVRANK": true, "ZCOUNT": true, "ZLEXCOUNT": true, "ZSCAN": true, "ZRANDMEMBER": true,
	"ZINTER": true, "ZUNION": true, "ZDIFF": true, "ZINTERCARD": true, "SCAN": true, "DBSIZE": true, "INFO": true, "PING": true,
	"ECHO": true, "TIME": true, "LASTSAVE": true, "ROLE": true, "RANDOMKEY": true, "DUMP": true, "BITCOUNT": true, "BITPOS": true,
	"GETBIT": true, "BITFIELD_RO": true, "PFCOUNT": true, "GEOPOS": true, "GEODIST": true, "GEOHASH": true, "GEOSEARCH": true,
	"GEORADIUS_RO": true, "GEORADIUSBYMEMBER_RO": true, "XRANGE": true, "XREVRANGE": true, "XLEN": true, "XREAD": true,
	"XPENDING": true, "XINFO": true, "OBJECT": true, "COMMAND": true, "KEYS": true, "LOLWUT": true, "SORT_RO": true, "TOUCH": true,
	"JSON.GET": true, "JSON.TYPE": true, "JSON.MGET": true, "JSON.STRLEN": true, "JSON.ARRLEN": true, "JSON.OBJKEYS": true,
	"FT._LIST": true, "FT.INFO": true, "FT.SEARCH": true, "FT.AGGREGATE": true, "TS.GET": true, "TS.RANGE": true, "TS.INFO": true,
}

// read-only subcommands of admin commands
var redisReadSub = map[string]map[string]bool{
	"CONFIG":   {"GET": true, "HELP": true},
	"SLOWLOG":  {"GET": true, "LEN": true, "HELP": true},
	"CLIENT":   {"LIST": true, "INFO": true, "GETNAME": true, "ID": true, "TRACKINGINFO": true, "GETREDIR": true, "HELP": true},
	"LATENCY":  {"LATEST": true, "HISTORY": true, "DOCTOR": true, "GRAPH": true, "HISTOGRAM": true, "HELP": true},
	"MEMORY":   {"USAGE": true, "STATS": true, "DOCTOR": true, "MALLOC-STATS": true, "HELP": true},
	"CLUSTER":  {"INFO": true, "NODES": true, "SLOTS": true, "SHARDS": true, "MYID": true, "KEYSLOT": true, "COUNTKEYSINSLOT": true, "GETKEYSINSLOT": true, "LINKS": true},
	"FUNCTION": {"LIST": true, "DUMP": true, "STATS": true, "HELP": true},
	"SCRIPT":   {"EXISTS": true, "HELP": true},
	"ACL":      {"LIST": true, "USERS": true, "WHOAMI": true, "CAT": true, "GETUSER": true, "HELP": true, "DRYRUN": true},
	"MODULE":   {"LIST": true, "HELP": true},
	"PUBSUB":   {"CHANNELS": true, "NUMSUB": true, "NUMPAT": true, "SHARDCHANNELS": true, "SHARDNUMSUB": true, "HELP": true},
}

// redisClass classifies a command: "blocked", "read" or "write".
func redisClass(args []string) string {
	if len(args) == 0 {
		return "blocked"
	}
	cmd := strings.ToUpper(args[0])
	if redisBlocked[cmd] {
		return "blocked"
	}
	if cmd == "CLIENT" && len(args) > 1 && strings.EqualFold(args[1], "REPLY") {
		return "blocked"
	}
	if cmd == "ACL" && len(args) > 1 && strings.EqualFold(args[1], "LOG") && (len(args) < 3 || !strings.EqualFold(args[2], "RESET")) {
		return "read"
	}
	if subs, ok := redisReadSub[cmd]; ok {
		if len(args) > 1 && subs[strings.ToUpper(args[1])] {
			return "read"
		}
		return "write"
	}
	if redisRead[cmd] {
		return "read"
	}
	return "write"
}

// redactRedis hides secrets in a command before it is audited.
func redactRedis(args []string) string {
	out := make([]string, len(args))
	copy(out, args)
	up := func(i int) string {
		if i < len(out) {
			return strings.ToUpper(out[i])
		}
		return ""
	}
	switch up(0) {
	case "CONFIG":
		if up(1) == "SET" {
			for i := 2; i+1 < len(out); i += 2 {
				k := strings.ToLower(out[i])
				if strings.Contains(k, "pass") || strings.Contains(k, "auth") || strings.Contains(k, "masteruser") {
					out[i+1] = "***"
				}
			}
		}
	case "ACL":
		for i := 2; i < len(out); i++ {
			if strings.HasPrefix(out[i], ">") || strings.HasPrefix(out[i], "#") || strings.HasPrefix(out[i], "<") || strings.HasPrefix(out[i], "!") {
				out[i] = out[i][:1] + "***"
			}
		}
	case "MIGRATE":
		for i := 0; i+1 < len(out); i++ {
			switch strings.ToUpper(out[i]) {
			case "AUTH":
				out[i+1] = "***"
			case "AUTH2":
				if i+2 < len(out) {
					out[i+2] = "***"
				}
			}
		}
	}
	for i, a := range out {
		out[i] = redisDisplayArg(a)
	}
	return shorten(strings.Join(out, " "), 1000)
}

// RedisResult is the reply of a console command.
type RedisResult struct {
	Command string `json:"command"`
	Output  string `json:"output"` // redis-cli formatted reply
	IsError bool   `json:"isError"`
	Write   bool   `json:"write"` // the command was classified as changing data/state
	Elapsed int64  `json:"elapsed"`
}

const redisKeysLimit = 10000

func (s *DatabaseService) redisCommand(ctx context.Context, conn *sshx.Conn, t Target, db int, args []string, allowWrite bool, sudoPW string) (RedisResult, error) {
	cls := redisClass(args)
	cmd := strings.ToUpper(args[0])
	switch cls {
	case "blocked":
		return RedisResult{}, apperr.New("db.redis.blocked", "cmd", cmd)
	case "write":
		if !allowWrite {
			return RedisResult{}, apperr.New("db.redis.needConfirm", "cmd", cmd)
		}
	}
	if cmd == "KEYS" && !allowWrite {
		p, err := s.runRedis(ctx, conn, t, db, [][]string{{"DBSIZE"}}, sudoPW)
		if err != nil {
			return RedisResult{}, err
		}
		if n := parseTTY(p[0]); n.kind == "int" && atoi64(n.s) > redisKeysLimit {
			return RedisResult{}, apperr.New("db.redis.keysLarge", "n", n.s)
		}
	}
	start := time.Now()
	p, err := s.runRedis(ctx, conn, t, db, [][]string{args}, sudoPW)
	if err != nil {
		return RedisResult{}, err
	}
	out := p[0]
	return RedisResult{Command: cmd, Output: out, IsError: strings.HasPrefix(out, "(error)"), Write: cls == "write",
		Elapsed: time.Since(start).Milliseconds()}, nil
}
