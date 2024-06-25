/*
MIT License

Copyright (c) 2023-2024 The Trzsz SSH Authors.

Permission is hereby granted, free of charge, to any person obtaining a copy
of this software and associated documentation files (the "Software"), to deal
in the Software without restriction, including without limitation the rights
to use, copy, modify, merge, publish, distribute, sublicense, and/or sell
copies of the Software, and to permit persons to whom the Software is
furnished to do so, subject to the following conditions:

The above copyright notice and this permission notice shall be included in all
copies or substantial portions of the Software.

THE SOFTWARE IS PROVIDED "AS IS", WITHOUT WARRANTY OF ANY KIND, EXPRESS OR
IMPLIED, INCLUDING BUT NOT LIMITED TO THE WARRANTIES OF MERCHANTABILITY,
FITNESS FOR A PARTICULAR PURPOSE AND NONINFRINGEMENT. IN NO EVENT SHALL THE
AUTHORS OR COPYRIGHT HOLDERS BE LIABLE FOR ANY CLAIM, DAMAGES OR OTHER
LIABILITY, WHETHER IN AN ACTION OF CONTRACT, TORT OR OTHERWISE, ARISING FROM,
OUT OF OR IN CONNECTION WITH THE SOFTWARE OR THE USE OR OTHER DEALINGS IN THE
SOFTWARE.
*/

package ser2net

import (
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/mattn/go-isatty"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

type stdinState struct {
	state    *term.State
	settings *string
}

func EnableVirtualTerminal() error {
	var inMode, outMode uint32
	inHandle, err := syscall.GetStdHandle(syscall.STD_INPUT_HANDLE)
	if err != nil {
		return err
	}
	if err := windows.GetConsoleMode(windows.Handle(inHandle), &inMode); err != nil {
		return err
	}
	restoreStdFuncs.Add(func() {
		windows.SetConsoleMode(windows.Handle(inHandle), inMode)
	})
	if err := windows.SetConsoleMode(windows.Handle(inHandle), inMode|windows.ENABLE_VIRTUAL_TERMINAL_INPUT); err != nil {
		return err
	}

	outHandle, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		return err
	}
	if err := windows.GetConsoleMode(windows.Handle(outHandle), &outMode); err != nil {
		return err
	}
	restoreStdFuncs.Add(func() {
		windows.SetConsoleMode(windows.Handle(outHandle), outMode)
	})
	if err := windows.SetConsoleMode(windows.Handle(outHandle),
		outMode|windows.ENABLE_VIRTUAL_TERMINAL_PROCESSING|windows.DISABLE_NEWLINE_AUTO_RETURN); err != nil {
		return err
	}

	return nil
}

var sttyCommandExists *bool

func sttyExecutable() bool {
	if sttyCommandExists == nil {
		_, err := exec.LookPath("stty")
		exists := err == nil && isatty.IsCygwinTerminal(os.Stdin.Fd()) // stty работает только если IsCygwinTerminal
		sttyCommandExists = &exists
	}
	return *sttyCommandExists
}

func sttySettings() (string, error) {
	cmd := exec.Command("stty", "-g")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return "", err
	}
	return strings.TrimSpace(string(out)), nil
}

func sttyMakeRaw() error {
	cmd := exec.Command("stty", "raw", "-echo")
	cmd.Stdin = os.Stdin
	return cmd.Run()
}

func sttyReset(settings string) {
	cmd := exec.Command("stty", settings)
	cmd.Stdin = os.Stdin
	_ = cmd.Run()
}

func sttySize() (int, int, error) {
	cmd := exec.Command("stty", "size")
	cmd.Stdin = os.Stdin
	out, err := cmd.Output()
	if err != nil {
		return 0, 0, err
	}
	output := strings.TrimSpace(string(out))
	tokens := strings.Fields(output)
	if len(tokens) != 2 {
		return 0, 0, fmt.Errorf("stty size invalid: %s", output)
	}
	rows, err := strconv.Atoi(tokens[0])
	if err != nil {
		return 0, 0, fmt.Errorf("stty size invalid: %s", output)
	}
	cols, err := strconv.Atoi(tokens[1])
	if err != nil {
		return 0, 0, fmt.Errorf("stty size invalid: %s", output)
	}
	return cols, rows, nil
}

func SetupVirtualTerminal() (restore func(), err error) {

	// enable virtual terminal
	if err := EnableVirtualTerminal(); err != nil {
		if !sttyExecutable() {
			return nil, fmt.Errorf("enable virtual terminal failed: %v try run it in MSYS2 or Cygwin", err)
		}
	}

	if isTerminal {
		state, err := MakeStdinRaw()
		if err != nil {
			return nil, err
		}
		restoreStdFuncs.Add(func() {
			resetStdin(state)
		})
	}

	return restoreStdFuncs.Cleanup, nil
}

func MakeStdinRaw() (*stdinState, error) {
	state, err := term.MakeRaw(int(os.Stdin.Fd()))
	if err == nil {
		return &stdinState{state, nil}, nil
	}

	if !sttyExecutable() {
		return nil, fmt.Errorf("terminal make raw failed: %v", err)
	}
	settings, err := sttySettings()
	if err != nil {
		return nil, fmt.Errorf("get stty settings failed: %v", err)
	}
	if err := sttyMakeRaw(); err != nil {
		return nil, fmt.Errorf("stty make raw failed: %v", err)
	}
	return &stdinState{nil, &settings}, nil
}

func resetStdin(s *stdinState) {
	if s.state != nil {
		_ = term.Restore(int(os.Stdin.Fd()), s.state)
		s.state = nil
	}
	if s.settings != nil {
		sttyReset(*s.settings)
		s.settings = nil
	}
}

func getTerminalSize() (int, int, error) {
	handle, err := syscall.GetStdHandle(syscall.STD_OUTPUT_HANDLE)
	if err != nil {
		if sttyExecutable() {
			return sttySize()
		}
		return 0, 0, err
	}
	var info windows.ConsoleScreenBufferInfo
	if err := windows.GetConsoleScreenBufferInfo(windows.Handle(handle), &info); err != nil {
		if sttyExecutable() {
			return sttySize()
		}
		return 0, 0, err
	}
	return int(info.Window.Right-info.Window.Left) + 1, int(info.Window.Bottom-info.Window.Top) + 1, nil
}

func onTerminalResize(setTerminalSize func(int, int)) {
	go func() {
		columns, rows, _ := getTerminalSize()
		for {
			time.Sleep(time.Second)
			width, height, err := getTerminalSize()
			if err != nil {
				continue
			}
			if columns != width || rows != height {
				columns = width
				rows = height
				setTerminalSize(width, height)
			}
		}
	}()
}

func getKeyboardInput() (*os.File, func(), error) {
	if isTerminal {
		return os.Stdin, func() {}, nil
	}

	path, err := syscall.UTF16PtrFromString("CONIN$")
	if err != nil {
		return nil, nil, err
	}
	handle, err := syscall.CreateFile(path, syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		syscall.FILE_SHARE_READ, nil, syscall.OPEN_EXISTING, 0, 0)
	if err != nil {
		return nil, nil, err
	}
	file := os.NewFile(uintptr(handle), "CONIN$")

	return file, func() { _ = file.Close() }, nil
}
