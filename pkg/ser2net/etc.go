//go:build !windows
// +build !windows

package ser2net

import (
	"os"

	"github.com/abakum/cancelreader"
	"github.com/google/shlex"
	"golang.org/x/sys/unix"
)

const (
	ArgC = "-c"
)

func size() (WinSize, error) {
	ws := WinSize{Width: 80, Height: 25}
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

// Для прерывания io.Copy(x, os.Stdin)
type Stdin struct {
	cancelreader.CancelReader
}

// Конструктор Stdin
func NewStdin() (*Stdin, error) {
	cr, err := cancelreader.NewReader(os.Stdin)
	return &Stdin{
		CancelReader: cr,
	}, err
}

// Деструктор Stdin
func (s *Stdin) Close() error {
	return s.CancelReader.Close()
}

// Прерывает io.Copy(x, os.Stdin)
func (s *Stdin) Cancel() bool {
	return s.CancelReader.Cancel()
}
