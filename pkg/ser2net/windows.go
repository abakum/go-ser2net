//go:build windows
// +build windows

package ser2net

import (
	"fmt"
	"io"
	"log"
	"os"
	"syscall"

	term "github.com/abakum/term/windows"
	"github.com/mattn/go-isatty"
	"golang.org/x/sys/windows"
)

const (
	ArgC = "/c"
)

func size() (WinSize, error) {
	var info windows.ConsoleScreenBufferInfo
	err := windows.GetConsoleScreenBufferInfo(windows.Handle(os.Stdout.Fd()), &info)
	if err != nil {
		return WinSize{Width: 80, Height: 25}, fmt.Errorf("unable to get console info: %w", err)
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

// Для прерывания io.Copy(x, os.Stdin)
type Stdin struct {
	io.ReadCloser
	closed bool
	cancel func() error
}

// Конструктор Stdin
func NewStdin() (*Stdin, error) {
	if isatty.IsCygwinTerminal(os.Stdin.Fd()) {
		return nil, fmt.Errorf("cygwin")
	}
	rc, err := term.NewAnsiReaderDuplicate(os.Stdin)
	return &Stdin{
		ReadCloser: rc,
	}, err
}

// Деструктор Stdin
func (s *Stdin) Close() error {
	if s.closed {
		return nil
	}
	if s.cancel != nil {
		return s.cancel()
	}
	return s.ReadCloser.Close()
}

// Прерывает io.Copy(x, os.Stdin)
func (s *Stdin) Cancel() (ok bool) {
	if s.closed {
		return false
	}
	if s.cancel != nil {
		return s.cancel() == nil
	}
	// Чтоб прерыватель сработал в Windows окно с ним должно получать события
	kernel32 := syscall.NewLazyDLL("kernel32.dll")
	user32 := syscall.NewLazyDLL("user32.dll")

	// https://docs.microsoft.com/en-us/windows/console/getconsolewindow
	GetConsoleWindows := kernel32.NewProc("GetConsoleWindow")
	// https://docs.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-showwindowasync
	ShowWindowAsync := user32.NewProc("ShowWindowAsync")
	// https://docs.microsoft.com/en-us/windows/win32/api/winuser/nf-winuser-setforegroundwindow
	SetForegroundWindow := user32.NewProc("SetForegroundWindow")

	consoleHandle, r2, err := GetConsoleWindows.Call()
	if consoleHandle == 0 {
		log.Printf("GetConsoleWindows %v %v\r\n", r2, err)
	} else {
		r1, r2, err := ShowWindowAsync.Call(consoleHandle, windows.SW_SHOW)
		if r1 == 0 {
			log.Printf("ShowWindowAsync @SW_SHOW %v %v\r\n", r2, err)
		} else {
			r1, r2, err := SetForegroundWindow.Call(consoleHandle)
			if r1 == 0 && err != nil && err.Error() != "The operation completed successfully." {
				log.Printf("SetForegroundWindow %v %v\r\n", r2, err)
			}
		}
	}
	s.closed = s.ReadCloser.Close() == nil
	return s.closed
}
