package monitor

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/mail"
	"net/smtp"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/db"
	"server-manager/internal/store"
)

// Channel is a notification destination. Secret material (bot tokens,
// webhook URLs, SMTP passwords, routing keys) lives in the OS keychain.
type Channel struct {
	ID      string `json:"id"`
	Name    string `json:"name"`
	Type    string `json:"type"` // telegram | slack | discord | webhook | email | pagerduty | desktop
	Enabled bool   `json:"enabled"`
	// Non-secret settings: chatId (telegram); url (webhook); host, port,
	// username, from, to, tls = starttls|tls|none (email).
	Config    map[string]string `json:"config"`
	HasSecret bool              `json:"hasSecret"`
	// Default channels receive every rule's notifications.
	Default bool `json:"default"`
}

func secretName(id string) string { return "mon:channel:" + id }

// desktopID is the implicit channel behind the "desktop notifications" setting.
const desktopID = "__desktop"

type notification struct {
	Alert Alert
	Kind  string // firing | reminder | resolved | test
}

// notifier batches notifications per channel (5 s window) and rate-limits
// each channel to 30 messages per hour, so an outage produces a handful of
// messages, not hundreds.
type notifier struct {
	c *core.Core

	mu      sync.Mutex
	pending map[string][]notification // channel id → queued
	timers  map[string]*time.Timer
	sent    map[string][]time.Time // channel id → send times (last hour)
	dropped map[string]int

	desktop func(title, body string) error
}

func newNotifier(c *core.Core) *notifier {
	return &notifier{c: c, pending: map[string][]notification{}, timers: map[string]*time.Timer{}, sent: map[string][]time.Time{}, dropped: map[string]int{}}
}

func (n *notifier) channels() []Channel {
	list, _ := db.List[Channel](n.c.DB, "mon.channel")
	return list
}

// send queues n for the given channels plus every default channel.
func (n *notifier) send(ids []string, msg notification) {
	targets := map[string]bool{}
	for _, id := range ids {
		targets[id] = true
	}
	for _, ch := range n.channels() {
		if ch.Default {
			targets[ch.ID] = true
		}
	}
	// "Desktop notifications" setting: every alert also pops up locally.
	if n.c.Settings().DesktopNotify && n.desktop != nil {
		targets[desktopID] = true
	}
	n.mu.Lock()
	defer n.mu.Unlock()
	for id := range targets {
		n.pending[id] = append(n.pending[id], msg)
		if n.timers[id] == nil {
			id := id
			n.timers[id] = time.AfterFunc(5*time.Second, func() { n.flush(id) })
		}
	}
}

func (n *notifier) flush(id string) {
	n.mu.Lock()
	msgs := n.pending[id]
	delete(n.pending, id)
	delete(n.timers, id)
	// Rate limit.
	now := time.Now()
	recent := n.sent[id][:0]
	for _, t := range n.sent[id] {
		if now.Sub(t) < time.Hour {
			recent = append(recent, t)
		}
	}
	n.sent[id] = recent
	if len(recent) >= 30 {
		n.dropped[id] += len(msgs)
		n.mu.Unlock()
		return
	}
	dropped := n.dropped[id]
	n.dropped[id] = 0
	n.sent[id] = append(n.sent[id], now)
	n.mu.Unlock()

	var ch *Channel
	if id == desktopID {
		ch = &Channel{ID: desktopID, Name: "desktop", Type: "desktop", Enabled: true}
	}
	for _, c := range n.channels() {
		if c.ID == id {
			c := c
			ch = &c
		}
	}
	if ch == nil || !ch.Enabled || len(msgs) == 0 {
		return
	}
	lang := n.c.Settings().NotifyLang
	title, body := render(lang, msgs, dropped)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := n.deliver(ctx, *ch, title, body, msgs); err != nil {
		n.c.AddEvent(core.Event{Kind: "alert", Severity: "warn", Code: "notify.failed", Params: map[string]string{"channel": ch.Name}, Detail: err.Error()})
	}
}

// Test sends a test message through a channel immediately.
func (n *notifier) Test(ch Channel) error {
	lang := n.c.Settings().NotifyLang
	title := tr(lang, "testTitle")
	body := tr(lang, "testBody")
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	return n.deliver(ctx, ch, title, body, []notification{{Kind: "test", Alert: Alert{Key: "test", RuleName: "test", Severity: "info", State: "firing", Fired: time.Now().UnixMilli()}}})
}

func (n *notifier) deliver(ctx context.Context, ch Channel, title, body string, msgs []notification) error {
	secret := store.Keychain(secretName(ch.ID))
	cfg := ch.Config
	if cfg == nil {
		cfg = map[string]string{}
	}
	switch ch.Type {
	case "telegram":
		if secret == "" || cfg["chatId"] == "" {
			return apperr.New("mon.channelIncomplete")
		}
		payload := map[string]any{"chat_id": cfg["chatId"], "text": "*" + mdEscape(title) + "*\n" + mdEscape(body), "parse_mode": "MarkdownV2", "disable_web_page_preview": true}
		return postJSON(ctx, "https://api.telegram.org/bot"+secret+"/sendMessage", payload, nil)
	case "slack":
		if secret == "" {
			return apperr.New("mon.channelIncomplete")
		}
		return postJSON(ctx, secret, map[string]any{"text": "*" + title + "*\n" + body}, nil)
	case "discord":
		if secret == "" {
			return apperr.New("mon.channelIncomplete")
		}
		content := "**" + title + "**\n" + body
		if len(content) > 1900 {
			content = content[:1900] + "…"
		}
		return postJSON(ctx, secret, map[string]any{"content": content, "username": "Server Manager"}, nil)
	case "webhook":
		u := cfg["url"]
		if u == "" {
			return apperr.New("mon.channelIncomplete")
		}
		alerts := []Alert{}
		for _, m := range msgs {
			alerts = append(alerts, m.Alert)
		}
		payload := map[string]any{"title": title, "text": body, "kind": msgs[0].Kind, "alerts": alerts, "sentAt": time.Now().UTC().Format(time.RFC3339)}
		headers := map[string]string{}
		if secret != "" {
			b, _ := json.Marshal(payload)
			mac := hmac.New(sha256.New, []byte(secret))
			mac.Write(b)
			headers["X-SM-Signature"] = "sha256=" + hex.EncodeToString(mac.Sum(nil))
			return postRaw(ctx, u, b, headers)
		}
		return postJSON(ctx, u, payload, headers)
	case "pagerduty":
		if secret == "" {
			return apperr.New("mon.channelIncomplete")
		}
		for _, m := range msgs {
			action := "trigger"
			if m.Kind == "resolved" {
				action = "resolve"
			}
			sev := map[string]string{"crit": "critical", "warn": "warning"}[m.Alert.Severity]
			if sev == "" {
				sev = "info"
			}
			ev := map[string]any{
				"routing_key": secret, "event_action": action, "dedup_key": "sm-" + m.Alert.Key,
				"payload": map[string]any{"summary": oneLine(lang(n), m), "source": m.Alert.ServerName, "severity": sev, "component": m.Alert.Target, "custom_details": m.Alert},
			}
			if err := postJSON(ctx, "https://events.pagerduty.com/v2/enqueue", ev, nil); err != nil {
				return err
			}
		}
		return nil
	case "email":
		return sendMail(ctx, cfg, secret, title, body)
	case "desktop":
		if n.desktop == nil {
			return apperr.New("mon.desktopUnavailable")
		}
		return n.desktop(title, body)
	}
	return apperr.New("mon.channelType", "type", ch.Type)
}

func lang(n *notifier) string { return n.c.Settings().NotifyLang }

var notifyHTTP = &http.Client{Timeout: 20 * time.Second}

func postJSON(ctx context.Context, u string, payload any, headers map[string]string) error {
	b, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	return postRaw(ctx, u, b, headers)
}

func postRaw(ctx context.Context, u string, body []byte, headers map[string]string) error {
	pu, err := url.Parse(u)
	if err != nil || (pu.Scheme != "https" && pu.Scheme != "http") || pu.Host == "" {
		return apperr.New("mon.invalidURL")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, u, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", "ServerManager/1.0")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := notifyHTTP.Do(req)
	if err != nil {
		// Never leak tokens embedded in URLs (telegram, webhooks).
		return apperr.New("mon.sendFailed").WithDetail(redactURL(err.Error()))
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 300 {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 500))
		return apperr.New("mon.sendFailed").WithDetail(fmt.Sprintf("HTTP %d: %s", resp.StatusCode, redactURL(strings.TrimSpace(string(msg)))))
	}
	return nil
}

var urlRe = regexp.MustCompile(`https?://[^\s"]+`)

func redactURL(s string) string {
	return urlRe.ReplaceAllStringFunc(s, func(u string) string {
		if p, err := url.Parse(u); err == nil {
			return p.Scheme + "://" + p.Host + "/…"
		}
		return "…"
	})
}

var mdSpecial = regexp.MustCompile(`([_*\[\]()~` + "`" + `>#+\-=|{}.!\\])`)

func mdEscape(s string) string { return mdSpecial.ReplaceAllString(s, `\$1`) }

func sendMail(ctx context.Context, cfg map[string]string, password, subject, body string) error {
	host, from, to := cfg["host"], cfg["from"], cfg["to"]
	if host == "" || from == "" || to == "" {
		return apperr.New("mon.channelIncomplete")
	}
	port := cfg["port"]
	if port == "" {
		port = "587"
	}
	if _, err := mail.ParseAddress(from); err != nil {
		return apperr.New("mon.invalidEmail", "email", from)
	}
	var rcpts []string
	for _, a := range strings.Split(to, ",") {
		a = strings.TrimSpace(a)
		if a == "" {
			continue
		}
		if _, err := mail.ParseAddress(a); err != nil {
			return apperr.New("mon.invalidEmail", "email", a)
		}
		rcpts = append(rcpts, a)
	}
	subject = strings.NewReplacer("\r", " ", "\n", " ").Replace(subject)
	msg := "From: " + from + "\r\nTo: " + strings.Join(rcpts, ", ") + "\r\nSubject: " + mimeHeader(subject) +
		"\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=UTF-8\r\nContent-Transfer-Encoding: 8bit\r\nDate: " +
		time.Now().Format(time.RFC1123Z) + "\r\n\r\n" + strings.ReplaceAll(body, "\n", "\r\n") + "\r\n"
	addr := net.JoinHostPort(host, port)
	d := net.Dialer{Timeout: 15 * time.Second}
	var conn net.Conn
	var err error
	mode := cfg["tls"]
	if mode == "tls" || (mode == "" && port == "465") {
		conn, err = tls.DialWithDialer(&d, "tcp", addr, &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12})
	} else {
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return apperr.New("mon.sendFailed").WithDetail(err.Error())
	}
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	cl, err := smtp.NewClient(conn, host)
	if err != nil {
		conn.Close()
		return apperr.New("mon.sendFailed").WithDetail(err.Error())
	}
	defer cl.Close()
	if mode != "tls" && mode != "none" && port != "465" {
		if ok, _ := cl.Extension("STARTTLS"); ok {
			if err := cl.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return apperr.New("mon.sendFailed").WithDetail(err.Error())
			}
		} else if mode == "starttls" {
			return apperr.New("mon.sendFailed").WithDetail("server does not support STARTTLS")
		}
	}
	if user := cfg["username"]; user != "" {
		if err := cl.Auth(smtp.PlainAuth("", user, password, host)); err != nil {
			return apperr.New("mon.sendFailed").WithDetail(err.Error())
		}
	}
	if err := cl.Mail(from); err != nil {
		return apperr.New("mon.sendFailed").WithDetail(err.Error())
	}
	for _, r := range rcpts {
		if err := cl.Rcpt(r); err != nil {
			return apperr.New("mon.sendFailed").WithDetail(err.Error())
		}
	}
	w, err := cl.Data()
	if err != nil {
		return apperr.New("mon.sendFailed").WithDetail(err.Error())
	}
	if _, err := w.Write([]byte(msg)); err != nil {
		return apperr.New("mon.sendFailed").WithDetail(err.Error())
	}
	if err := w.Close(); err != nil {
		return apperr.New("mon.sendFailed").WithDetail(err.Error())
	}
	return cl.Quit()
}

func mimeHeader(s string) string {
	for _, r := range s {
		if r > 127 {
			return "=?UTF-8?B?" + b64(s) + "?="
		}
	}
	return s
}

// ---- channel CRUD ----

var chanTypes = map[string]bool{"telegram": true, "slack": true, "discord": true, "webhook": true, "email": true, "pagerduty": true, "desktop": true}

func (n *notifier) save(ch Channel, secret string, setSecret bool) (Channel, error) {
	if !chanTypes[ch.Type] {
		return ch, apperr.New("mon.channelType", "type", ch.Type)
	}
	ch.Name = strings.TrimSpace(ch.Name)
	if ch.Name == "" {
		ch.Name = ch.Type
	}
	if ch.Config == nil {
		ch.Config = map[string]string{}
	}
	for k, v := range ch.Config {
		ch.Config[k] = strings.TrimSpace(v)
	}
	if u := ch.Config["url"]; ch.Type == "webhook" {
		if p, err := url.Parse(u); err != nil || (p.Scheme != "https" && p.Scheme != "http") || p.Host == "" {
			return ch, apperr.New("mon.invalidURL")
		}
	}
	if setSecret && (ch.Type == "slack" || ch.Type == "discord") && secret != "" {
		if p, err := url.Parse(secret); err != nil || p.Scheme != "https" {
			return ch, apperr.New("mon.invalidURL")
		}
	}
	if ch.Type == "email" {
		if p := ch.Config["port"]; p != "" {
			if v, err := strconv.Atoi(p); err != nil || v < 1 || v > 65535 {
				return ch, apperr.New("mon.invalidPort")
			}
		}
	}
	if ch.ID == "" {
		ch.ID = uuid.NewString()
	} else {
		var old Channel
		if n.c.DB.Get("mon.channel", ch.ID, &old) == nil {
			ch.HasSecret = old.HasSecret
		}
	}
	if setSecret {
		if err := store.SetKeychain(secretName(ch.ID), secret); err != nil {
			return ch, err
		}
		ch.HasSecret = secret != ""
	}
	return ch, n.c.DB.Put("mon.channel", ch.ID, ch)
}

func (n *notifier) delete(id string) error {
	_ = store.SetKeychain(secretName(id), "")
	return n.c.DB.Delete("mon.channel", id)
}

// ---- message rendering (vi / en) ----

var texts = map[string]map[string]string{
	"vi": {
		"firing": "🔴 CẢNH BÁO", "reminder": "🔁 VẪN ĐANG CẢNH BÁO", "resolved": "✅ ĐÃ ỔN ĐỊNH", "crit": "NGHIÊM TRỌNG", "warn": "CẢNH BÁO",
		"many": "{n} cảnh báo", "since": "từ", "dropped": "({n} thông báo khác bị giới hạn tần suất)",
		"testTitle": "Server Manager: thông báo thử", "testBody": "Kênh thông báo hoạt động bình thường ✓",
		"unreachable": "không kết nối được", "service_down": "dịch vụ ngừng hoạt động", "http_down": "health check HTTP lỗi", "container_unhealthy": "container không khoẻ",
	},
	"en": {
		"firing": "🔴 ALERT", "reminder": "🔁 STILL FIRING", "resolved": "✅ RESOLVED", "crit": "CRITICAL", "warn": "WARNING",
		"many": "{n} alerts", "since": "since", "dropped": "({n} more notifications were rate-limited)",
		"testTitle": "Server Manager: test notification", "testBody": "The notification channel works ✓",
		"unreachable": "unreachable", "service_down": "service down", "http_down": "HTTP health check failing", "container_unhealthy": "container unhealthy",
	},
}

func tr(lang, key string) string {
	if m, ok := texts[lang]; ok {
		if v, ok := m[key]; ok {
			return v
		}
	}
	return texts["en"][key]
}

// builtinNames localizes the default rules' names (unless renamed).
var builtinNames = map[string][3]string{ // id → {english default, vi, en}
	"builtin-cpu-85":                {"CPU > 85% (5m)", "CPU > 85% (5 phút)", "CPU > 85% (5m)"},
	"builtin-memory-90":             {"RAM > 90% (5m)", "RAM > 90% (5 phút)", "RAM > 90% (5m)"},
	"builtin-disk-80":               {"Disk > 80%", "Ổ đĩa > 80%", "Disk > 80%"},
	"builtin-disk-90":               {"Disk > 90%", "Ổ đĩa > 90%", "Disk > 90%"},
	"builtin-load_per_core-2":       {"Load / core > 2 (5m)", "Load / nhân > 2 (5 phút)", "Load / core > 2 (5m)"},
	"builtin-service_down-0":        {"Service down", "Dịch vụ ngừng hoạt động", "Service down"},
	"builtin-unreachable-0":         {"Server unreachable", "Server không kết nối được", "Server unreachable"},
	"builtin-ssl_days-14":           {"SSL expires < 14 days", "Chứng chỉ SSL hết hạn < 14 ngày", "SSL expires < 14 days"},
	"builtin-http_down-0":           {"HTTP health check failed", "Health check HTTP lỗi", "HTTP health check failed"},
	"builtin-container_unhealthy-0": {"Container unhealthy", "Container không khoẻ", "Container unhealthy"},
	"builtin-backup_age-26":         {"Backup older than 26h", "Bản sao lưu cũ hơn 26 giờ", "Backup older than 26h"},
}

func ruleName(lang, id, name string) string {
	if n, ok := builtinNames[id]; ok && n[0] == name {
		if lang == "vi" {
			return n[1]
		}
		return n[2]
	}
	return name
}

func oneLine(lang string, m notification) string {
	al := m.Alert
	what := ruleName(lang, al.Rule, al.RuleName)
	if metricInfo[al.Metric].bool {
		what = tr(lang, al.Metric)
	}
	s := al.ServerName + ": " + what
	if al.Target != "" {
		s += " [" + al.Target + "]"
	}
	if v := fmtValue(al.Metric, al.Value); v != "" {
		if m.Kind == "resolved" {
			s += " — " + v
		} else {
			s += " — " + v + " " + al.Op + " " + fmtValue(al.Metric, al.Threshold)
		}
	}
	return s
}

func render(lang string, msgs []notification, dropped int) (string, string) {
	var title string
	if len(msgs) == 1 {
		m := msgs[0]
		title = tr(lang, m.Kind)
		if m.Kind != "resolved" {
			title += " · " + tr(lang, m.Alert.Severity)
		}
		title += " · " + m.Alert.ServerName
	} else {
		title = "Server Manager · " + strings.ReplaceAll(tr(lang, "many"), "{n}", strconv.Itoa(len(msgs)))
	}
	var b strings.Builder
	for _, m := range msgs {
		prefix := ""
		if len(msgs) > 1 {
			prefix = tr(lang, m.Kind) + " "
		}
		b.WriteString(prefix + oneLine(lang, m))
		if m.Alert.Since > 0 && m.Kind != "resolved" {
			b.WriteString(" (" + tr(lang, "since") + " " + time.UnixMilli(m.Alert.Since).Format("15:04 02/01") + ")")
		}
		b.WriteString("\n")
	}
	if dropped > 0 {
		b.WriteString(strings.ReplaceAll(tr(lang, "dropped"), "{n}", strconv.Itoa(dropped)))
	}
	return title, strings.TrimSpace(b.String())
}
