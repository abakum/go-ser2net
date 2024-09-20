//go:build windows
// +build windows

package ser2net

import (
	"fmt"
	"os"

	"golang.org/x/sys/windows"
)

const (
	ArgC = "/c"
)

func size() (WinSize, error) {
	var info windows.ConsoleScreenBufferInfo
	err := windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info)
	if err != nil {
		return WinSize{}, fmt.Errorf("unable to get console info: %w", err)
	}

	winsize := WinSize{
		Width:  uint16(info.Window.Right - info.Window.Left + 1),
		Height: uint16(info.Window.Bottom - info.Window.Top + 1),
	}

	return winsize, nil
}

func splitCommandLine(command string) ([]string, error) {
	return windows.DecomposeCommandLine(command)
}
