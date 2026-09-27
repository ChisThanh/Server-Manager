package web

import (
	"fmt"
	"path"
	"strconv"
	"strings"
)

// ---- Caddyfile parsing ----

type cdyNode struct {
	Tokens   []string
	Line     int
	HasBlock bool
	Children []*cdyNode
}

// cdyLines tokenizes a Caddyfile into lines of tokens.
func cdyLines(src string) [][]ngxToken {
	var lines [][]ngxToken
	var cur []ngxToken
	line := 1
	i, n := 0, len(src)
	endLine := func() {
		if len(cur) > 0 {
			lines = append(lines, cur)
		}
		cur = nil
	}
	for i < n {
		c := src[i]
		switch {
		case c == '\n':
			endLine()
			line++
			i++
		case c == ' ' || c == '\t' || c == '\r':
			i++
		case c == '#':
			for i < n && src[i] != '\n' {
				i++
			}
		case c == '\\' && i+1 < n && src[i+1] == '\n':
			line++
			i += 2 // line continuation
		case c == '"':
			start := line
			i++
			var b strings.Builder
			for i < n && src[i] != '"' {
				if src[i] == '\\' && i+1 < n && (src[i+1] == '"' || src[i+1] == '\\') {
					b.WriteByte(src[i+1])
					i += 2
					continue
				}
				if src[i] == '\n' {
					line++
				}
				b.WriteByte(src[i])
				i++
			}
			i++
			cur = append(cur, ngxToken{text: b.String(), quoted: true, line: start})
		case c == '`':
			start := line
			i++
			var b strings.Builder
			for i < n && src[i] != '`' {
				if src[i] == '\n' {
					line++
				}
				b.WriteByte(src[i])
				i++
			}
			i++
			cur = append(cur, ngxToken{text: b.String(), quoted: true, line: start})
		default:
			var b strings.Builder
			start := line
			for i < n && src[i] != ' ' && src[i] != '\t' && src[i] != '\r' && src[i] != '\n' {
				b.WriteByte(src[i])
				i++
			}
			cur = append(cur, ngxToken{text: b.String(), line: start})
		}
	}
	endLine()
	return lines
}

func cdyParse(src string) []*cdyNode {
	lines := cdyLines(src)
	pos := 0
	var parse func(depth int) []*cdyNode
	parse = func(depth int) []*cdyNode {
		out := []*cdyNode{}
		for pos < len(lines) {
			l := lines[pos]
			pos++
			if len(l) == 1 && !l[0].quoted && l[0].text == "}" {
				return out
			}
			nd := &cdyNode{Line: l[0].line}
			for _, t := range l {
				nd.Tokens = append(nd.Tokens, t.text)
			}
			last := l[len(l)-1]
			if !last.quoted && last.text == "{" {
				nd.Tokens = nd.Tokens[:len(nd.Tokens)-1]
				nd.HasBlock = true
				if depth < 30 {
					nd.Children = parse(depth + 1)
				}
			}
			out = append(out, nd)
		}
		return out
	}
	return parse(0)
}

func cdyFindDeep(list []*cdyNode, name string) []*cdyNode {
	var out []*cdyNode
	for _, n := range list {
		if len(n.Tokens) > 0 && n.Tokens[0] == name {
			out = append(out, n)
		}
		if n.HasBlock {
			out = append(out, cdyFindDeep(n.Children, name)...)
		}
	}
	return out
}

// cdySites returns the site blocks of a parsed Caddyfile (skipping the
// global options block and snippets), plus top-level import arguments.
func cdySites(nodes []*cdyNode) (sites []*cdyNode, imports []string, snippets map[string]bool) {
	snippets = map[string]bool{}
	for _, n := range nodes {
		switch {
		case len(n.Tokens) == 0 && n.HasBlock:
			// global options
		case len(n.Tokens) > 0 && strings.HasPrefix(n.Tokens[0], "(") && n.HasBlock:
			snippets[strings.Trim(n.Tokens[0], "()")] = true
		case len(n.Tokens) > 0 && n.Tokens[0] == "import" && !n.HasBlock:
			if len(n.Tokens) > 1 {
				imports = append(imports, n.Tokens[1])
			}
		case n.HasBlock:
			sites = append(sites, n)
		}
	}
	// A Caddyfile holding a single site may omit the braces.
	if len(sites) == 0 && len(nodes) > 0 && !nodes[0].HasBlock && len(nodes[0].Tokens) > 0 &&
		nodes[0].Tokens[0] != "import" && strings.ContainsAny(nodes[0].Tokens[0], ".:") {
		sites = append(sites, &cdyNode{Tokens: nodes[0].Tokens, Line: nodes[0].Line, HasBlock: true, Children: nodes[1:]})
	}
	return sites, imports, snippets
}

// summarizeCaddySite turns a site block into a ServerBlock.
func summarizeCaddySite(n *cdyNode) ServerBlock {
	sb := ServerBlock{Line: n.Line, ServerNames: []string{}, Listens: []Listen{}, ProxyPass: []string{}}
	ports := map[int]bool{}
	for _, tok := range n.Tokens {
		for _, a := range strings.Split(tok, ",") {
			a = strings.TrimSpace(a)
			if a == "" {
				continue
			}
			scheme := ""
			if i := strings.Index(a, "://"); i >= 0 {
				scheme = a[:i]
				a = a[i+3:]
			}
			if i := strings.Index(a, "/"); i >= 0 {
				a = a[:i]
			}
			host, port := a, 0
			if i := strings.LastIndex(a, ":"); i >= 0 && !strings.HasSuffix(a, "]") {
				host = a[:i]
				port, _ = strconv.Atoi(a[i+1:])
			}
			if host != "" {
				sb.ServerNames = append(sb.ServerNames, host)
			}
			switch {
			case port != 0:
				ports[port] = port == 443 || (scheme == "https")
			case scheme == "http":
				ports[80] = false
			default:
				ports[80] = false
				ports[443] = true
			}
		}
	}
	for p, ssl := range ports {
		sb.Listens = append(sb.Listens, Listen{Port: p, SSL: ssl, HTTP2: ssl})
	}
	for _, rp := range cdyFindDeep(n.Children, "reverse_proxy") {
		for _, t := range rp.Tokens[1:] {
			if strings.HasPrefix(t, "/") || t == "*" {
				continue // matcher
			}
			if strings.HasPrefix(t, "@") {
				continue
			}
			sb.ProxyPass = append(sb.ProxyPass, t)
		}
		for _, c := range rp.Children {
			if len(c.Tokens) > 1 && c.Tokens[0] == "to" {
				sb.ProxyPass = append(sb.ProxyPass, c.Tokens[1:]...)
			}
		}
	}
	for _, r := range cdyFindDeep(n.Children, "root") {
		if len(r.Tokens) > 1 {
			sb.Root = r.Tokens[len(r.Tokens)-1]
		}
	}
	for _, r := range cdyFindDeep(n.Children, "redir") {
		args := r.Tokens[1:]
		if len(args) > 0 && (strings.HasPrefix(args[0], "@") || (strings.HasPrefix(args[0], "/") && len(args) > 1)) {
			args = args[1:]
		}
		if len(args) > 0 {
			code := "302"
			if len(args) > 1 {
				code = args[1]
				if code == "permanent" {
					code = "301"
				} else if code == "temporary" {
					code = "302"
				}
			}
			sb.Return = code + " " + args[0]
		}
	}
	for _, t := range cdyFindDeep(n.Children, "tls") {
		sb.TLS = strings.Join(t.Tokens[1:], " ")
		if len(t.Tokens) == 3 && strings.HasPrefix(t.Tokens[1], "/") {
			sb.SSLCert, sb.SSLKey = t.Tokens[1], t.Tokens[2]
		}
	}
	return sb
}

// ---- Caddyfile generation ----

// cdyQ quotes a Caddyfile token. Placeholders ({…}) stay active, as users
// expect in header values; quotes/backslashes are escaped.
func cdyQ(s string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"`
}

type cdyGenOpts struct {
	Name       string
	AuthHashes map[string]string // user → bcrypt hash
	BasicAuthV string            // "basicauth" (< 2.8) | "basic_auth"
	LogDir     string
}

const leStagingCA = "https://acme-staging-v02.api.letsencrypt.org/directory"

func genCaddy(sp SiteSpec, o cdyGenOpts) genOutput {
	var b strings.Builder
	w := func(indent int, format string, args ...any) {
		b.WriteString(strings.Repeat("\t", indent))
		fmt.Fprintf(&b, format, args...)
		b.WriteByte('\n')
	}
	b.WriteString(managedMarker + "\n")
	b.WriteString(metaPrefix + encodeMeta(sp) + "\n")
	b.WriteString("# Generated by Server Manager. Edit this site in the app: manual changes are replaced when it is saved again.\n\n")

	addrs := make([]string, len(sp.Domains))
	for i, d := range sp.Domains {
		if sp.SSL == SSLNone {
			addrs[i] = "http://" + d
		} else {
			addrs[i] = d
		}
	}
	w(0, "%s {", strings.Join(addrs, ", "))
	switch sp.SSL {
	case SSLLetsEncrypt:
		switch {
		case sp.Staging:
			if sp.Email != "" {
				w(1, "tls %s {", sp.Email)
			} else {
				w(1, "tls {")
			}
			w(2, "ca %s", leStagingCA)
			w(1, "}")
		case sp.Email != "":
			w(1, "tls %s", sp.Email)
		}
	case SSLInternal:
		w(1, "tls internal")
	case SSLCustom:
		w(1, "tls %s %s", cdyQ(sp.CertPath), cdyQ(sp.KeyPath))
	}
	if sp.Gzip {
		w(1, "encode gzip zstd")
	}
	if sp.HSTS || len(sp.ResponseHeaders) > 0 {
		w(1, "header {")
		if sp.HSTS {
			v := "max-age=" + strconv.Itoa(sp.HSTSMaxAge)
			if sp.HSTSSubdomains {
				v += "; includeSubDomains"
			}
			w(2, "Strict-Transport-Security %s", cdyQ(v))
		}
		for _, h := range sp.ResponseHeaders {
			w(2, "%s %s", h.Name, cdyQ(h.Value))
		}
		w(1, "}")
	}
	if sp.MaxBodyMB > 0 {
		w(1, "request_body {")
		w(2, "max_size %dMB", sp.MaxBodyMB)
		w(1, "}")
	}
	if sp.BasicAuth {
		dir := o.BasicAuthV
		if dir == "" {
			dir = "basicauth"
		}
		w(1, "%s {", dir)
		for _, u := range sp.AuthUsers {
			h := o.AuthHashes[u.User]
			if h == "" {
				h = "$2a$14$(bcrypt-hash-generated-when-applied)"
			}
			w(2, "%s %s", u.User, h)
		}
		w(1, "}")
	}
	if sp.AccessLog != "off" {
		p := sp.AccessLog
		if p == "" {
			p = path.Join(o.LogDir, o.Name+".access.log")
		}
		w(1, "log {")
		w(2, "output file %s", cdyQ(p))
		w(1, "}")
	}
	if sp.Extra != "" {
		b.WriteByte('\n')
		w(1, "# Extra directives")
		for _, l := range strings.Split(sp.Extra, "\n") {
			if strings.TrimSpace(l) == "" {
				b.WriteByte('\n')
				continue
			}
			w(1, "%s", strings.TrimRight(l, " \t"))
		}
	}
	b.WriteByte('\n')
	switch sp.Type {
	case TypeProxy:
		ups := []string{}
		https := false
		for _, raw := range sp.Upstreams {
			u, _ := parseUpstream(raw)
			switch u.Scheme {
			case "unix":
				ups = append(ups, "unix/"+u.Socket)
			case "https":
				https = true
				ups = append(ups, "https://"+u.HostPort())
			default:
				ups = append(ups, u.HostPort())
			}
		}
		needBlock := sp.LBMethod != "" || len(sp.RequestHeaders) > 0 || sp.ConnectTimeout > 0 || sp.ReadTimeout > 0 || sp.SendTimeout > 0
		if !needBlock {
			w(1, "reverse_proxy %s", strings.Join(ups, " "))
			break
		}
		w(1, "reverse_proxy %s {", strings.Join(ups, " "))
		if sp.LBMethod != "" {
			w(2, "lb_policy %s", sp.LBMethod)
		}
		for _, h := range sp.RequestHeaders {
			w(2, "header_up %s %s", h.Name, cdyQ(h.Value))
		}
		if sp.ConnectTimeout > 0 || sp.ReadTimeout > 0 || sp.SendTimeout > 0 {
			w(2, "transport http {")
			if sp.ConnectTimeout > 0 {
				w(3, "dial_timeout %ds", sp.ConnectTimeout)
			}
			if sp.ReadTimeout > 0 {
				w(3, "read_timeout %ds", sp.ReadTimeout)
			}
			if sp.SendTimeout > 0 {
				w(3, "write_timeout %ds", sp.SendTimeout)
			}
			if https {
				w(3, "tls")
			}
			w(2, "}")
		}
		w(1, "}")
	case TypeStatic:
		w(1, "root * %s", cdyQ(sp.Root))
		if sp.SPA {
			w(1, "try_files {path} {path}/ /index.html")
		}
		w(1, "file_server {")
		w(2, "index %s", sp.Index)
		w(1, "}")
	case TypeRedirect:
		target := sp.RedirectTo
		if sp.PreservePath {
			target = strings.TrimRight(target, "/") + "{uri}"
		}
		w(1, "redir %s %d", cdyQ(target), sp.RedirectCode)
	}
	w(0, "}")
	return genOutput{Content: b.String()}
}
