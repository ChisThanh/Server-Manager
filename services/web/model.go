package web

// Engines, site types and SSL modes of the managed-site form.
const (
	EngineNginx = "nginx"
	EngineCaddy = "caddy"

	TypeProxy    = "proxy"
	TypeStatic   = "static"
	TypeRedirect = "redirect"

	SSLNone        = "none"
	SSLLetsEncrypt = "letsencrypt"
	SSLCustom      = "custom"
	SSLInternal    = "internal" // Caddy's local CA
)

// Header is one custom request (proxy_set_header / header_up) or response
// (add_header / header) header.
type Header struct {
	Name  string `json:"name"`
	Value string `json:"value"`
}

// AuthUser is a basic-auth account. Password is write-only: it is hashed
// with bcrypt when the site is applied and never stored or returned.
type AuthUser struct {
	User     string `json:"user"`
	Password string `json:"password,omitempty"`
}

// SiteSpec is the form model of an app-managed site. It is stored (without
// passwords) in the generated file's "# sm-meta:" line so editing round-trips.
type SiteSpec struct {
	Engine  string   `json:"engine"`  // nginx | caddy
	Domains []string `json:"domains"` // first = primary; the rest are aliases
	Type    string   `json:"type"`    // proxy | static | redirect

	// Reverse proxy
	Upstreams []string `json:"upstreams"` // http://127.0.0.1:3000, https://h:8443, unix:/run/app.sock
	LBMethod  string   `json:"lbMethod"`  // "" (round robin) | least_conn | ip_hash
	WebSocket bool     `json:"webSocket"`
	// Timeouts in seconds (0 = engine default).
	ConnectTimeout int      `json:"connectTimeout"`
	ReadTimeout    int      `json:"readTimeout"`
	SendTimeout    int      `json:"sendTimeout"`
	RequestHeaders []Header `json:"requestHeaders"`

	// Static site
	Root  string `json:"root"`
	Index string `json:"index"`
	SPA   bool   `json:"spa"`

	// Redirect
	RedirectTo   string `json:"redirectTo"`
	RedirectCode int    `json:"redirectCode"` // 301 | 302 | 307 | 308
	PreservePath bool   `json:"preservePath"`

	// TLS
	SSL            string `json:"ssl"`      // none | letsencrypt | custom | internal (caddy)
	CertName       string `json:"certName"` // Let's Encrypt lineage / custom certificate name
	CertPath       string `json:"certPath"` // custom: fullchain path
	KeyPath        string `json:"keyPath"`  // custom: private key path
	Email          string `json:"email"`    // Let's Encrypt account e-mail
	KeyType        string `json:"keyType"`  // ecdsa | rsa
	Staging        bool   `json:"staging"`
	ForceHTTPS     bool   `json:"forceHttps"`
	HTTP2          bool   `json:"http2"`
	HSTS           bool   `json:"hsts"`
	HSTSMaxAge     int    `json:"hstsMaxAge"`
	HSTSSubdomains bool   `json:"hstsSubdomains"`

	// Misc
	ResponseHeaders []Header   `json:"responseHeaders"`
	MaxBodyMB       int        `json:"maxBodyMb"` // 0 = default
	Gzip            bool       `json:"gzip"`
	RateLimit       bool       `json:"rateLimit"`
	RateRPS         int        `json:"rateRps"`
	RateBurst       int        `json:"rateBurst"`
	RateNoDelay     bool       `json:"rateNoDelay"`
	BasicAuth       bool       `json:"basicAuth"`
	AuthRealm       string     `json:"authRealm"`
	AuthUsers       []AuthUser `json:"authUsers"`
	AccessLog       string     `json:"accessLog"` // "" = default path, "off" = disabled
	ErrorLog        string     `json:"errorLog"`
	Extra           string     `json:"extra"` // raw directives added to the main server block
}

// Listen is one listen directive of an nginx server block.
type Listen struct {
	Addr          string `json:"addr"`
	Port          int    `json:"port"`
	SSL           bool   `json:"ssl"`
	HTTP2         bool   `json:"http2"`
	DefaultServer bool   `json:"defaultServer"`
}

// ServerBlock summarizes one server { } (nginx) or site block (caddy).
type ServerBlock struct {
	Line        int      `json:"line"`
	ServerNames []string `json:"serverNames"`
	Listens     []Listen `json:"listens"`
	Root        string   `json:"root"`
	ProxyPass   []string `json:"proxyPass"`
	Return      string   `json:"return"`
	SSLCert     string   `json:"sslCert"`
	SSLKey      string   `json:"sslKey"`
	TLS         string   `json:"tls"` // caddy: tls directive arguments
}

// Site is a configuration file holding one or more virtual hosts.
type Site struct {
	ID        string        `json:"id"` // absolute file path
	Engine    string        `json:"engine"`
	File      string        `json:"file"`
	Name      string        `json:"name"` // display name (primary domain / file name)
	Managed   bool          `json:"managed"`
	Enabled   bool          `json:"enabled"`
	CanToggle bool          `json:"canToggle"` // lives in sites-available / conf.d / caddy sites dir
	Domains   []string      `json:"domains"`
	Kind      string        `json:"kind"`   // proxy | static | redirect | other
	Target    string        `json:"target"` // upstream / root / redirect URL
	HTTPS     bool          `json:"https"`
	HTTP2     bool          `json:"http2"`
	Default   bool          `json:"default"`
	Certs     []string      `json:"certs"` // certificate file paths referenced
	Blocks    []ServerBlock `json:"blocks"`
	Spec      *SiteSpec     `json:"spec"` // managed sites only
	// PendingCert: managed with Let's Encrypt but the certificate isn't
	// issued yet (served over plain HTTP until then).
	PendingCert bool `json:"pendingCert"`
}

// EngineInfo is what detection found about one web server.
type EngineInfo struct {
	Name      string `json:"name"` // nginx | caddy | apache | traefik
	Installed bool   `json:"installed"`
	Running   bool   `json:"running"`
	Enabled   bool   `json:"enabled"` // systemd unit enabled
	Version   string `json:"version"`
	Binary    string `json:"binary"`
	Service   string `json:"service"`
	// Editable: the app can create/edit sites for it (nginx, caddy).
	Editable bool   `json:"editable"`
	Layout   string `json:"layout"` // nginx: debian | confd; caddy: caddyfile
	Config   string `json:"config"` // main config file
	// Result of nginx -t / caddy validate ("" = not run).
	TestOK     bool   `json:"testOk"`
	TestRun    bool   `json:"testRun"`
	TestOutput string `json:"testOutput"`
	Docker     bool   `json:"docker"` // traefik: runs as a container
	// Caddy: whether /etc/caddy/Caddyfile imports sites/*.caddy.
	ImportsSites bool `json:"importsSites"`
}

// WebStatus is the detection result shown at the top of the panel.
type WebStatus struct {
	Engines    []EngineInfo `json:"engines"`
	Certbot    bool         `json:"certbot"`
	CertbotVer string       `json:"certbotVersion"`
	PkgManager string       `json:"pkgManager"` // apt | dnf | yum | apk | ""
	Systemd    bool         `json:"systemd"`
	Listeners  []string     `json:"listeners"` // processes listening on :80/:443
	Webroot    string       `json:"webroot"`
}

// ApplyResult is returned by every change to a site configuration.
type ApplyResult struct {
	ID          string   `json:"id"`
	File        string   `json:"file"`
	TestOutput  string   `json:"testOutput"`
	Reloaded    bool     `json:"reloaded"`
	NotRunning  bool     `json:"notRunning"` // engine stopped: validated only
	PendingCert bool     `json:"pendingCert"`
	Warnings    []string `json:"warnings"`
}

// HistoryEntry is a previous version of a site file (kept for undo).
type HistoryEntry struct {
	TS      int64  `json:"ts"` // unix ms
	Action  string `json:"action"`
	Actor   string `json:"actor"`
	Content string `json:"content"`
}

// Preview is the generated configuration for a spec.
type Preview struct {
	File    string `json:"file"`
	Content string `json:"content"`
	// Extra files written alongside (rate-limit zone, websocket map…).
	Extras      []PreviewFile `json:"extras"`
	PendingCert bool          `json:"pendingCert"`
}

type PreviewFile struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}
