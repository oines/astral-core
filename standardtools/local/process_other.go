//go:build !unix && !windows

package local

import (
	"context"
	"errors"

	"github.com/oines/astral-core/standardtools"
)

func defaultShell() string { return "" }

func startProcess(context.Context, string, string, []string, standardtools.ProcessRequest) (standardtools.Process, error) {
	return nil, beforeStart(errors.New("local shell/PTY backend currently requires Unix; provide a native Backend for this platform"))
}
