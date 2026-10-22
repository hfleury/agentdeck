// Package notify shows desktop notifications through the freedesktop
// notification service (notify-send / libnotify), and rings the terminal bell.
package notify

import (
	"os"
	"os/exec"
)

// Disabled turns notifications off (tests set AGENTDECK_NOTIFY=off).
func Disabled() bool { return os.Getenv("AGENTDECK_NOTIFY") == "off" }

// Send shows a desktop notification. Missing notify-send is not an error:
// the bell still marks the terminal window.
func Send(title, body string, urgent bool) {
	if Disabled() {
		return
	}
	bin, err := exec.LookPath("notify-send")
	if err != nil {
		return
	}
	u := "normal"
	if urgent {
		u = "critical"
	}
	_ = exec.Command(bin, "-a", "agentdeck", "-u", u, "-i", "utilities-terminal", title, body).Start()
}

// Bell writes BEL to each terminal; most Wayland compositors and X11 window
// managers turn it into an urgency hint on the window.
func Bell(ttys []string) {
	if Disabled() {
		return
	}
	for _, t := range ttys {
		if f, err := os.OpenFile(t, os.O_WRONLY, 0); err == nil {
			_, _ = f.Write([]byte{'\a'})
			f.Close()
		}
	}
}
