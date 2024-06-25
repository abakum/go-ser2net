package ser2net

import (
	"os"

	"github.com/mattn/go-isatty"
)

type afterDo []func()

func (a *afterDo) Cleanup() {
	for i := len(*a) - 1; i >= 0; i-- {
		(*a)[i]()
	}
	*a = afterDo{}
}

func (a *afterDo) Add(f func()) {
	*a = append(*a, f)
}

var (
	restoreStdFuncs afterDo
	isTerminal      bool = isatty.IsTerminal(os.Stdin.Fd()) || isatty.IsCygwinTerminal(os.Stdin.Fd())
)
