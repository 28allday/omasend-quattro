// Package notify raises desktop notifications via libnotify's notify-send,
// which on Omarchy/Hyprland is displayed by the mako daemon. It is best-effort:
// when notify-send is absent or there is no graphical session bus (a genuinely
// headless box), it silently does nothing, so the receiver never blocks or
// errors on a machine with no desktop.
package notify

import (
	"context"
	"os"
	"os/exec"
	"time"

	"omasend/internal/dbg"
)

// appName is shown as the originating application in the notification daemon,
// and iconName is the bundled hicolor icon the installer drops in (falls back to
// no icon if the theme lacks it — harmless).
const (
	appName  = "Omarchy-Send"
	iconName = "omarchy-send"
)

// Available reports whether a desktop notification can plausibly be shown:
// notify-send is on PATH and we appear to be inside a graphical session. The
// TUI calls this once at startup to decide whether to enable notifications.
func Available() bool {
	if _, err := exec.LookPath("notify-send"); err != nil {
		return false
	}
	return os.Getenv("WAYLAND_DISPLAY") != "" ||
		os.Getenv("DISPLAY") != "" ||
		os.Getenv("DBUS_SESSION_BUS_ADDRESS") != ""
}

// Send shows a desktop notification with the given summary and body. It returns
// immediately; the notify-send invocation runs in the background (bounded by a
// short timeout) and any failure is recorded only in the debug log. It is a
// no-op when Available reports false, so it is safe to call unconditionally.
func Send(summary, body string) {
	if !Available() {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "notify-send",
			"-a", appName,
			"-i", iconName,
			summary, body)
		if err := cmd.Run(); err != nil {
			dbg.Logf("notify-send failed: %v", err)
		}
	}()
}
