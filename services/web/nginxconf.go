package web

import (
	"path"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// A small parser for the nginx configuration language, following the rules
// of ngx_conf_read_token: words separated by whitespace, ';' '{' '}' as
// delimiters, "…" and '…' strings with backslash escapes, '#' comments at
// the start of a token and ${var} inside words.

type ngxDirective struct {
	Name     string
	Args     []string
	Block    []*ngxDirective
	HasBlock bool
	Line     int
}

type ngxToken struct {
	text   string
	quoted bool
	line   int
}

func ngxTokenize(src string) []ngxToken {
	var toks []ngxToken
	line := 1
	i := 0
	n := len(src)
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == ';' || c == '{' || c == '}':
			toks = append(toks, ngxToken{text: string(c), line: line})
			i++
		case c == '"' || c == '\'':
			q := c
			start := line
			i++
			var b strings.Builder
			for i < n && src[i] != q {
				if src[i] == '\\' && i+1 < n {
					nx := src[i+1]
					switch nx {
					case '"', '\'', '\\':
						b.WriteByte(nx)
					case 'n':
						b.WriteByte('\n')
					case 't':
						b.WriteByte('\t')
					case 'r':
						b.WriteByte('\r')
					default:
						b.WriteByte('\\')
						b.WriteByte(nx)
					}
					if nx == '\n' {
						line++
					}
					i += 2
					continue
				}
				if src[i] == '\n' {
					line++
				}
				b.WriteByte(src[i])
				i++
			}
			i++ // closing quote
			toks = append(toks, ngxToken{text: b.String(), quoted: true, line: start})
		default:
			var b strings.Builder
			start := line
			for i < n {
				c := src[i]
				if c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == ';' || c == '{' || c == '}' {
					// "${" inside a word is variable syntax, not a block.
					if c == '{' && b.Len() > 0 && strings.HasSuffix(b.String(), "$") {
						for i < n && src[i] != '}' {
							b.WriteByte(src[i])
							i++
						}
						if i < n {
							b.WriteByte('}')
							i++
						}
						continue
					}
					break
				}
				if c == '\\' && i+1 < n {
					b.WriteByte(c)
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				b.WriteByte(c)
				i++
			}
			toks = append(toks, ngxToken{text: b.String(), line: start})
		}
	}
	return toks
}

// ngxParse builds the directive tree. It is lenient: unbalanced input
// yields what could be parsed.
func ngxParse(src string) []*ngxDirective {
	toks := ngxTokenize(src)
	pos := 0
	var parse func(depth int) []*ngxDirective
	parse = func(depth int) []*ngxDirective {
		out := []*ngxDirective{}
		var cur *ngxDirective
		for pos < len(toks) {
			t := toks[pos]
			pos++
			if !t.quoted {
				switch t.text {
				case ";":
					if cur != nil {
						out = append(out, cur)
						cur = nil
					}
					continue
				case "{":
					if cur == nil {
						cur = &ngxDirective{Line: t.line}
					}
					cur.HasBlock = true
					if depth < 50 {
						cur.Block = parse(depth + 1)
					}
					out = append(out, cur)
					cur = nil
					continue
				case "}":
					if cur != nil {
						out = append(out, cur)
					}
					return out
				}
			}
			if cur == nil {
				cur = &ngxDirective{Name: t.text, Line: t.line}
			} else {
				cur.Args = append(cur.Args, t.text)
			}
		}
		if cur != nil {
			out = append(out, cur)
		}
		return out
	}
	return parse(0)
}

func (d *ngxDirective) arg(i int) string {
	if i < len(d.Args) {
		return d.Args[i]
	}
	return ""
}

// find returns directives named name in list (not recursive).
func ngxFind(list []*ngxDirective, name string) []*ngxDirective {
	var out []*ngxDirective
	for _, d := range list {
		if d.Name == name {
			out = append(out, d)
		}
	}
	return out
}

// ngxFindDeep returns directives named name at any depth.
func ngxFindDeep(list []*ngxDirective, name string) []*ngxDirective {
	var out []*ngxDirective
	for _, d := range list {
		if d.Name == name {
			out = append(out, d)
		}
		if d.HasBlock {
			out = append(out, ngxFindDeep(d.Block, name)...)
		}
	}
	return out
}

// ---- nginx -T dump ----

var ngxFileMarker = regexp.MustCompile(`^# configuration file (.+):$`)

type ngxFile struct {
	Path    string
	Content string
}

// splitNginxDump splits `nginx -T` output into its files, in order.
func splitNginxDump(out string) []ngxFile {
	var files []ngxFile
	var cur *ngxFile
	var b strings.Builder
	flush := func() {
		if cur != nil {
			cur.Content = strings.TrimSuffix(b.String(), "\n")
			files = append(files, *cur)
		}
		b.Reset()
	}
	for _, line := range strings.Split(out, "\n") {
		if m := ngxFileMarker.FindStringSubmatch(strings.TrimRight(line, "\r")); m != nil {
			flush()
			cur = &ngxFile{Path: m[1]}
			continue
		}
		if cur != nil {
			b.WriteString(line)
			b.WriteByte('\n')
		}
	}
	flush()
	return files
}

// ngxServer is an http server block and the file it lives in.
type ngxServer struct {
	File  string
	Block *ngxDirective
}

type ngxConfig struct {
	Files     []ngxFile
	Servers   []ngxServer
	Upstreams map[string][]string // http upstream name → servers
	User      string              // worker user
	// Files reached through includes in the http context.
	HTTPFiles map[string]bool
	// Absolute include patterns used in the http context, in order.
	HTTPIncludes []string
}

// analyzeNginx walks the dump from the main file, following includes (by
// matching their glob against the dumped file names) and recording the
// context each server block appears in.
func analyzeNginx(files []ngxFile) *ngxConfig {
	cfg := &ngxConfig{Files: files, Upstreams: map[string][]string{}, HTTPFiles: map[string]bool{}}
	if len(files) == 0 {
		return cfg
	}
	byPath := map[string]*ngxFile{}
	paths := []string{}
	for i := range files {
		byPath[files[i].Path] = &files[i]
		paths = append(paths, files[i].Path)
	}
	prefix := path.Dir(files[0].Path)
	visited := map[string]int{}
	var walk func(list []*ngxDirective, file string, ctx string, depth int)
	walk = func(list []*ngxDirective, file string, ctx string, depth int) {
		if depth > 20 {
			return
		}
		for _, d := range list {
			switch {
			case d.Name == "include" && len(d.Args) == 1:
				pat := d.Args[0]
				if !strings.HasPrefix(pat, "/") {
					pat = path.Join(prefix, pat)
				}
				matched := []string{}
				for _, p := range paths {
					if ok, _ := path.Match(pat, p); ok || p == pat {
						matched = append(matched, p)
					}
				}
				sort.Strings(matched)
				if ctx == "http" {
					cfg.HTTPIncludes = append(cfg.HTTPIncludes, pat)
				}
				for _, p := range matched {
					if visited[p] > 3 {
						continue
					}
					visited[p]++
					if ctx == "http" {
						cfg.HTTPFiles[p] = true
					}
					walk(ngxParse(byPath[p].Content), p, ctx, depth+1)
				}
			case d.Name == "user" && ctx == "main" && len(d.Args) > 0:
				cfg.User = d.Args[0]
			case d.Name == "http" && d.HasBlock:
				walk(d.Block, file, "http", depth+1)
			case d.Name == "server" && d.HasBlock && ctx == "http":
				cfg.Servers = append(cfg.Servers, ngxServer{File: file, Block: d})
			case d.Name == "upstream" && d.HasBlock && ctx == "http" && len(d.Args) > 0:
				var srv []string
				for _, s := range ngxFind(d.Block, "server") {
					if len(s.Args) > 0 {
						srv = append(srv, s.Args[0])
					}
				}
				cfg.Upstreams[d.Args[0]] = srv
			case d.HasBlock && (d.Name == "events" || d.Name == "stream" || d.Name == "mail"):
				walk(d.Block, file, d.Name, depth+1)
			}
		}
	}
	visited[files[0].Path] = 1
	walk(ngxParse(files[0].Content), files[0].Path, "main", 0)
	return cfg
}

// summarizeServer extracts the interesting parts of a server block.
func summarizeServer(d *ngxDirective) ServerBlock {
	sb := ServerBlock{Line: d.Line, ServerNames: []string{}, Listens: []Listen{}, ProxyPass: []string{}}
	http2On := false
	for _, x := range ngxFind(d.Block, "http2") {
		if x.arg(0) == "on" {
			http2On = true
		}
	}
	for _, x := range ngxFind(d.Block, "server_name") {
		for _, a := range x.Args {
			if a != "" && a != `""` {
				sb.ServerNames = append(sb.ServerNames, a)
			}
		}
	}
	for _, x := range ngxFind(d.Block, "listen") {
		if len(x.Args) == 0 {
			continue
		}
		l := parseListen(x.Args)
		if http2On {
			l.HTTP2 = true
		}
		sb.Listens = append(sb.Listens, l)
	}
	if len(sb.Listens) == 0 {
		sb.Listens = append(sb.Listens, Listen{Port: 80})
	}
	if r := ngxFind(d.Block, "root"); len(r) > 0 {
		sb.Root = r[0].arg(0)
	}
	if sb.Root == "" {
		for _, loc := range ngxFind(d.Block, "location") {
			if loc.arg(len(loc.Args)-1) == "/" {
				if r := ngxFind(loc.Block, "root"); len(r) > 0 {
					sb.Root = r[0].arg(0)
				}
			}
		}
	}
	for _, p := range ngxFindDeep(d.Block, "proxy_pass") {
		if len(p.Args) > 0 {
			sb.ProxyPass = append(sb.ProxyPass, p.Args[0])
		}
	}
	// A return at server level, or in "location /".
	rets := ngxFind(d.Block, "return")
	for _, loc := range ngxFind(d.Block, "location") {
		if len(loc.Args) > 0 && loc.Args[len(loc.Args)-1] == "/" {
			rets = append(rets, ngxFind(loc.Block, "return")...)
		}
	}
	for _, r := range rets {
		if len(r.Args) >= 2 {
			sb.Return = r.Args[0] + " " + r.Args[1]
		} else if len(r.Args) == 1 {
			sb.Return = r.Args[0]
		}
	}
	if c := ngxFind(d.Block, "ssl_certificate"); len(c) > 0 {
		sb.SSLCert = c[0].arg(0)
	}
	if c := ngxFind(d.Block, "ssl_certificate_key"); len(c) > 0 {
		sb.SSLKey = c[0].arg(0)
	}
	return sb
}

func parseListen(args []string) Listen {
	l := Listen{}
	addr := args[0]
	switch {
	case strings.HasPrefix(addr, "unix:"):
		l.Addr = addr
	case strings.HasPrefix(addr, "["):
		if i := strings.LastIndex(addr, "]:"); i >= 0 {
			l.Addr = addr[:i+1]
			l.Port, _ = strconv.Atoi(addr[i+2:])
		} else {
			l.Addr = addr
			l.Port = 80
		}
	default:
		if p, err := strconv.Atoi(addr); err == nil {
			l.Port = p
		} else if i := strings.LastIndex(addr, ":"); i >= 0 {
			l.Addr = addr[:i]
			l.Port, _ = strconv.Atoi(addr[i+1:])
		} else {
			l.Addr = addr
			l.Port = 80
		}
	}
	for _, a := range args[1:] {
		switch a {
		case "ssl":
			l.SSL = true
		case "http2":
			l.HTTP2 = true
		case "default_server", "default":
			l.DefaultServer = true
		}
	}
	return l
}

// isHTTPSRedirect reports whether a return value is the usual
// "redirect everything to https" (so it doesn't make a site a redirect).
func isHTTPSRedirect(ret string) bool {
	return strings.Contains(ret, "https://$host") || strings.Contains(ret, "https://$server_name")
}

// buildSite aggregates a file's server blocks into a Site.
func buildSite(engine, file string, blocks []ServerBlock, upstreams map[string][]string) Site {
	st := Site{ID: file, Engine: engine, File: file, Domains: []string{}, Certs: []string{}, Blocks: blocks, Kind: "other"}
	seen := map[string]bool{}
	certSeen := map[string]bool{}
	var proxy, root, ret string
	for _, b := range blocks {
		for _, n := range b.ServerNames {
			if n != "_" && n != "localhost" && !seen[n] {
				seen[n] = true
				st.Domains = append(st.Domains, n)
			}
		}
		for _, l := range b.Listens {
			if l.SSL {
				st.HTTPS = true
			}
			if l.HTTP2 {
				st.HTTP2 = true
			}
			if l.DefaultServer {
				st.Default = true
			}
		}
		if b.SSLCert != "" && !certSeen[b.SSLCert] {
			certSeen[b.SSLCert] = true
			st.Certs = append(st.Certs, b.SSLCert)
			st.HTTPS = true
		}
		if proxy == "" && len(b.ProxyPass) > 0 {
			proxy = b.ProxyPass[0]
		}
		if root == "" && b.Root != "" {
			root = b.Root
		}
		if ret == "" && b.Return != "" && !isHTTPSRedirect(b.Return) {
			ret = b.Return
		}
	}
	switch {
	case proxy != "":
		st.Kind = TypeProxy
		st.Target = proxy
		if host := upstreamName(proxy); host != "" {
			if srv, ok := upstreams[host]; ok && len(srv) > 0 {
				st.Target = strings.Join(srv, ", ")
			}
		}
	case ret != "":
		st.Kind = TypeRedirect
		st.Target = ret
	case root != "":
		st.Kind = TypeStatic
		st.Target = root
	}
	if len(st.Domains) > 0 {
		st.Name = st.Domains[0]
	} else {
		st.Name = path.Base(file)
	}
	return st
}

// upstreamName returns the host part of a proxy_pass URL.
func upstreamName(p string) string {
	s := p
	if i := strings.Index(s, "://"); i >= 0 {
		s = s[i+3:]
	} else {
		return ""
	}
	if i := strings.IndexAny(s, "/:"); i >= 0 {
		s = s[:i]
	}
	return s
}
