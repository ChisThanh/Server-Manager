package core

import "errors"

// Settings are app-wide preferences stored in the local database.
type Settings struct {
	// OperatorName identifies the person using this app in the audit log
	// (e.g. an email). Empty = OS account.
	OperatorName string `json:"operatorName"`
	// CollectInterval is the metrics sampling period in seconds.
	CollectInterval int `json:"collectInterval"`
	// BackgroundMode keeps the app running in the menu bar / tray when the
	// window is closed so monitoring, alerts and scheduled backups go on.
	BackgroundMode bool `json:"backgroundMode"`
	// TerminalIdleMinutes closes terminals idle this long (0 = never).
	TerminalIdleMinutes int `json:"terminalIdleMinutes"`
	// RecordTerminal saves terminal sessions (asciicast) for later review.
	RecordTerminal bool `json:"recordTerminal"`
	// DesktopNotify shows OS notifications for alerts.
	DesktopNotify bool `json:"desktopNotify"`
	// ConfirmProduction requires typing the server name before dangerous
	// actions on production servers.
	ConfirmProduction bool `json:"confirmProduction"`
	// Retention (days, 0 = forever).
	AuditRetentionDays int `json:"auditRetentionDays"`
	EventRetentionDays int `json:"eventRetentionDays"`
	// CommandConcurrency limits parallel servers in the command center.
	CommandConcurrency int `json:"commandConcurrency"`
	// NotifyLang is the language of alert notifications (vi | en).
	NotifyLang string `json:"notifyLang"`
}

func defaultSettings() Settings {
	return Settings{
		CollectInterval:     15,
		DesktopNotify:       true,
		ConfirmProduction:   true,
		EventRetentionDays:  90,
		AuditRetentionDays:  0,
		TerminalIdleMinutes: 0,
		CommandConcurrency:  5,
		NotifyLang:          "vi",
	}
}

func (s *Settings) normalize() {
	if s.CollectInterval < 5 {
		s.CollectInterval = 5
	}
	if s.CollectInterval > 600 {
		s.CollectInterval = 600
	}
	if s.CommandConcurrency < 1 {
		s.CommandConcurrency = 1
	}
	if s.CommandConcurrency > 50 {
		s.CommandConcurrency = 50
	}
	if s.TerminalIdleMinutes < 0 {
		s.TerminalIdleMinutes = 0
	}
	if s.EventRetentionDays < 0 {
		s.EventRetentionDays = 0
	}
	if s.AuditRetentionDays < 0 {
		s.AuditRetentionDays = 0
	}
	if s.NotifyLang != "en" {
		s.NotifyLang = "vi"
	}
}

func (c *Core) loadSettings() {
	s := defaultSettings()
	if err := c.DB.Get("settings", "app", &s); err != nil && !errors.Is(err, errNotFound()) {
		logf("load settings: %v", err)
	}
	s.normalize()
	c.settingsMu.Lock()
	c.settings = s
	c.settingsMu.Unlock()
}

func (c *Core) Settings() Settings {
	c.settingsMu.RLock()
	defer c.settingsMu.RUnlock()
	return c.settings
}

// OnSettingsChange registers a listener called after settings change.
func (c *Core) OnSettingsChange(fn func(Settings)) {
	c.settingsMu.Lock()
	c.onSettings = append(c.onSettings, fn)
	c.settingsMu.Unlock()
}

func (c *Core) SaveSettings(s Settings) (Settings, error) {
	s.normalize()
	if err := c.DB.Put("settings", "app", s); err != nil {
		return Settings{}, err
	}
	c.settingsMu.Lock()
	c.settings = s
	fns := append([]func(Settings){}, c.onSettings...)
	c.settingsMu.Unlock()
	for _, fn := range fns {
		fn(s)
	}
	return s, nil
}
