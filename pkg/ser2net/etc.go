//go:build !windows
// +build !windows

package ser2net

import (
	"os"

	"github.com/google/shlex"
	"golang.org/x/sys/unix"
)

const (
	ArgC = "-c"
)

func size() (WinSize, error) {
	var ws WinSize
	uws, err := unix.IoctlGetWinsize(int(os.Stdout.Fd()), unix.TIOCGWINSZ)
	if err != nil {
		return ws, err
	}
	// Translate from unix.Winsize to console.WinSize
	ws.Height = uws.Row
	ws.Width = uws.Col
	return ws, nil
}

func splitCommandLine(command string) ([]string, error) {
	return shlex.Split(command)
}
