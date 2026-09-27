package main

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"log"
	"os/exec"
	"runtime"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"github.com/wailsapp/wails/v3/pkg/application"
	"github.com/wailsapp/wails/v3/pkg/events"
	"github.com/wailsapp/wails/v3/pkg/services/notifications"

	"server-manager/internal/apperr"
	corepkg "server-manager/internal/core"
	"server-manager/services"
	"server-manager/services/backup"
	"server-manager/services/command"
	"server-manager/services/database"
	"server-manager/services/deploy"
	"server-manager/services/docker"
	"server-manager/services/logs"
	"server-manager/services/monitor"
	"server-manager/services/security"
	"server-manager/services/web"
)

//go:embed all:frontend/dist
var assets embed.FS

// Tray icons: a black-and-alpha template for the macOS menu bar (it follows
// light/dark automatically) and the colour logo for Windows/Linux.
//
//go:embed build/tray/tray-template.png
var trayTemplateIcon []byte

//go:embed build/tray/tray.png
var trayIcon []byte

func main() {
	core, err := services.NewCore()
	if err != nil {
		log.Fatal(err)
	}

	// Native notifications need a bundle identifier on macOS, which dev and
	// unpackaged builds don't have; it is started on demand and falls back to
	// the OS command line tools.
	notifier := notifications.New()
	var notifyOK atomic.Bool
	mon := monitor.New(core)
	webSvc := web.New(core)
	monitor.SetCertSource(mon, func(id string) ([]monitor.CertInfo, error) {
		certs, err := webSvc.Certificates(id, "")
		out := make([]monitor.CertInfo, 0, len(certs))
		for _, c := range certs {
			if !c.Staging {
				out = append(out, monitor.CertInfo{Name: c.Name, Domains: c.Domains, NotAfter: c.NotAfter})
			}
		}
		return out, err
	})
	monitor.SetDesktopNotifier(mon, func(title, body string) error {
		if notifyOK.Load() {
			if err := notifier.SendNotification(notifications.NotificationOptions{ID: uuid.NewString(), Title: title, Body: body}); err == nil {
				return nil
			}
		}
		return osNotify(title, body)
	})

	app := application.New(application.Options{
		Name:        "Server Manager",
		Description: "Agentless server management over SSH",
		// Coded errors travel to the frontend as the call error's `cause`,
		// where they are translated into the user's language.
		MarshalError: func(err error) []byte {
			var ae *apperr.Error
			if !errors.As(err, &ae) {
				return nil
			}
			b, _ := json.Marshal(ae)
			return b
		},
		Services: []application.Service{
			application.NewService(services.NewServerService(core)),
			application.NewService(services.NewFileService(core)),
			application.NewService(services.NewTerminalService(core)),
			application.NewService(services.NewSystemService(core)),
			application.NewService(services.NewAppService(core)),
			application.NewService(mon),
			application.NewService(docker.New(core)),
			application.NewService(logs.New(core)),
			application.NewService(security.New(core)),
			application.NewService(webSvc),
			application.NewService(database.New(core)),
			application.NewService(backup.New(core)),
			application.NewService(deploy.New(core)),
			application.NewService(command.New(core)),
		},
		Assets: application.AssetOptions{
			Handler: application.AssetFileServerFS(assets),
		},
		Mac: application.MacOptions{
			// In background mode closing the window only hides it (see below).
			ApplicationShouldTerminateAfterLastWindowClosed: true,
		},
	})

	win := app.Window.NewWithOptions(application.WebviewWindowOptions{
		Title:          "Server Manager",
		Width:          1360,
		Height:         860,
		MinWidth:       900,
		MinHeight:      560,
		EnableFileDrop: true,
		Mac: application.MacWindow{
			InvisibleTitleBarHeight: 38,
			Backdrop:                application.MacBackdropNormal,
			TitleBar:                application.MacTitleBarHiddenInset,
		},
		BackgroundColour: application.NewRGB(24, 24, 27),
		URL:              "/",
	})

	// Files dragged from the OS onto an element marked data-file-drop-target
	// are forwarded to the frontend, which uploads them into the directory
	// named by that element's data-remote-dir attribute.
	win.OnWindowEvent(events.Common.WindowFilesDropped, func(e *application.WindowEvent) {
		ev := services.FileDropEvent{Files: e.Context().DroppedFiles(), Target: map[string]string{}}
		if d := e.Context().DropTargetDetails(); d != nil {
			ev.Target = d.Attributes
		}
		app.Event.Emit(services.EventFileDrop, ev)
	})

	// Background mode: closing the window hides it and a tray icon keeps
	// the app (monitoring, alerts, scheduled backups) running.
	win.RegisterHook(events.Common.WindowClosing, func(e *application.WindowEvent) {
		if core.Settings().BackgroundMode {
			e.Cancel()
			win.Hide()
		}
	})
	app.Event.OnApplicationEvent(events.Mac.ApplicationShouldHandleReopen, func(*application.ApplicationEvent) {
		win.Show()
		win.Focus()
	})
	var tray *application.SystemTray
	syncTray := func(bg bool, lang string) {
		application.InvokeSync(func() {
			if !bg {
				if tray != nil {
					tray.Destroy()
					tray = nil
				}
				return
			}
			if tray != nil {
				return
			}
			label := map[bool][2]string{true: {"Mở Server Manager", "Thoát"}, false: {"Open Server Manager", "Quit"}}[lang == "vi"]
			tray = app.SystemTray.New()
			if runtime.GOOS == "darwin" {
				tray.SetTemplateIcon(trayTemplateIcon)
			} else {
				tray.SetIcon(trayIcon)
			}
			tray.SetTooltip("Server Manager")
			menu := app.NewMenu()
			menu.Add(label[0]).OnClick(func(*application.Context) {
				win.Show()
				win.Focus()
			})
			menu.AddSeparator()
			menu.Add(label[1]).OnClick(func(*application.Context) { app.Quit() })
			tray.SetMenu(menu)
		})
	}
	app.Event.OnApplicationEvent(events.Common.ApplicationStarted, func(*application.ApplicationEvent) {
		st := core.Settings()
		syncTray(st.BackgroundMode, st.NotifyLang)
		go func() {
			if notifier.ServiceStartup(context.Background(), application.ServiceOptions{}) == nil {
				if ok, _ := notifier.RequestNotificationAuthorization(); ok {
					notifyOK.Store(true)
				}
			}
		}()
	})
	core.OnSettingsChange(func(st corepkg.Settings) { syncTray(st.BackgroundMode, st.NotifyLang) })

	err = app.Run()
	core.Close()
	if err != nil {
		log.Fatal(err)
	}
}

// osNotify shows a desktop notification with the OS tools (fallback).
func osNotify(title, body string) error {
	switch runtime.GOOS {
	case "darwin":
		q := func(s string) string { return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`).Replace(s) + `"` }
		return exec.Command("osascript", "-e", "display notification "+q(body)+" with title "+q(title)).Run()
	case "linux":
		return exec.Command("notify-send", "-a", "Server Manager", title, body).Run()
	}
	return errors.New("desktop notifications unavailable")
}
