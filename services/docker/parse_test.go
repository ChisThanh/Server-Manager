package docker

import "testing"

func TestParseSize(t *testing.T) {
	cases := map[string]int64{
		"0B": 0, "572B": 572, "1.5kB": 1500, "1.64MB": 1640000, "2GB": 2e9,
		"1.109MiB": 1162870, "7.818GiB": 8394513580, "0B (virtual 1.64MB)": 0, "N/A": 0, "": 0, "12x": 0,
	}
	for in, want := range cases {
		if got := parseSize(in); got != want {
			t.Errorf("parseSize(%q) = %d, want %d", in, got, want)
		}
	}
	if a, b := parsePair("1.109MiB / 7.818GiB"); a != 1162870 || b != 8394513580 {
		t.Errorf("parsePair: %d %d", a, b)
	}
}

func TestParsePSAndStats(t *testing.T) {
	out := `{"id":"abc","names":"web-1","image":"nginx:1","command":"\"nginx -g\"","created":"2026-09-25 03:37:35 +0000 UTC","state":"running","status":"Up 3 minutes (unhealthy)","ports":"0.0.0.0:80->80/tcp","networks":"a,b","project":"shop","service":"web","workdir":"/srv/shop","files":"/srv/shop/compose.yaml,/srv/shop/compose.prod.yaml","envfiles":""}
garbage
{"id":"def","names":"old","image":"x","command":"","created":"","state":"","status":"Exited (0) 2 hours ago","ports":"","networks":"","project":"","service":"","workdir":"","files":"","envfiles":""}`
	cs := parsePS(out)
	if len(cs) != 2 {
		t.Fatalf("%+v", cs)
	}
	old, web := cs[0], cs[1]
	if web.Health != "unhealthy" || web.Command != "nginx -g" || len(web.ConfigFiles) != 2 || len(web.Networks) != 2 || web.Created != 1790307455 {
		t.Errorf("web %+v", web)
	}
	if old.State != "exited" || old.Networks == nil || old.EnvFiles == nil {
		t.Errorf("old %+v", old)
	}
	st := parseStats(`{"BlockIO":"1MB / 2MB","CPUPerc":"12.50%","ID":"abc","MemPerc":"0.01%","MemUsage":"1MiB / 1GiB","Name":"web-1","NetIO":"572B / 1kB","PIDs":"3"}`)
	if len(st) != 1 || st[0].CPU != 12.5 || st[0].MemUsage != 1<<20 || st[0].MemLimit != 1<<30 || st[0].NetTx != 1000 || st[0].BlockWrite != 2e6 || st[0].PIDs != 3 {
		t.Errorf("stats %+v", st)
	}
}

func TestComposePSFormats(t *testing.T) {
	line := `{"Name":"p-web-1","Service":"web","State":"running","Status":"Up","Health":"","Publishers":[{"URL":"0.0.0.0","TargetPort":80,"PublishedPort":8080,"Protocol":"tcp"},{"URL":"::","TargetPort":80,"PublishedPort":8080,"Protocol":"tcp"}]}`
	for _, out := range []string{line, "[" + line + "]"} {
		cs := parseComposePS(out)
		if len(cs) != 1 || cs[0].Ports != "0.0.0.0:8080->80/tcp, :::8080->80/tcp" {
			t.Errorf("%q → %+v", out, cs)
		}
	}
}

func TestValidators(t *testing.T) {
	for _, ok := range []string{"nginx", "nginx:1.25-alpine", "ghcr.io/org/app:v1", "localhost:5000/a/b", "redis@sha256:" + hex64, "sha256:" + hex64, "5a197254c58f"} {
		if checkImage(ok) != nil {
			t.Errorf("image %q rejected", ok)
		}
	}
	for _, bad := range []string{"-x", "Nginx", "a b", "a;b", "a:tag with space", "$(id)", ""} {
		if checkImage(bad) == nil {
			t.Errorf("image %q accepted", bad)
		}
	}
	for _, ok := range []string{"/srv/app/compose.yaml", "/opt/my stack/docker-compose.yml"} {
		if _, err := checkPath(ok); err != nil {
			t.Errorf("path %q rejected", ok)
		}
	}
	for _, bad := range []string{"srv/x", "/srv/../etc", "/a,b", "/a'b", "/a\nb", "/", "/srv/-rf", "/a\"b", "/a$b"} {
		if _, err := checkPath(bad); err == nil {
			t.Errorf("path %q accepted", bad)
		}
	}
	for _, ok := range []string{"15m", "2h", "1790307455", "2026-09-25", "2026-09-25T10:00:00Z"} {
		if !sinceRe.MatchString(ok) {
			t.Errorf("since %q rejected", ok)
		}
	}
}

const hex64 = "5a197254c58ff6e4d327b8d628f0abec931789e63354dd797a8f47d21bbbce90"
