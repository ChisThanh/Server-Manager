// Package services contains the Wails-bound services the frontend talks to.
// Shared state and helpers live in internal/core.
package services

import (
	"context"

	"github.com/wailsapp/wails/v3/pkg/application"

	"server-manager/internal/apperr"
	"server-manager/internal/core"
	"server-manager/internal/sshx"
)

// Core is the shared state behind all services.
type Core = core.Core

func NewCore() (*Core, error) { return core.New() }

// Event names emitted to the frontend.
const (
	EventTermData = "term:data"
	EventTermExit = "term:exit"
	EventTransfer = "transfer:progress"
	EventFileDrop = "files:dropped"
)

type TermDataEvent struct {
	ID   string `json:"id"`
	Data string `json:"data"` // base64
}

type TermExitEvent struct {
	ID    string        `json:"id"`
	Error *apperr.Error `json:"error"`
}

type TransferEvent struct {
	ID        string        `json:"id"`
	ConnID    string        `json:"connId"`
	Kind      string        `json:"kind"` // upload | download
	Name      string        `json:"name"`
	More      int           `json:"more"` // number of additional top-level items
	Current   string        `json:"current"`
	Done      int64         `json:"done"`
	Total     int64         `json:"total"`
	Files     int           `json:"files"`
	FilesDone int           `json:"filesDone"`
	State     string        `json:"state"` // running | done | error | cancelled
	Error     *apperr.Error `json:"error"`
	Target    string        `json:"target"`
}

type FileDropEvent struct {
	Files  []string          `json:"files"`
	Target map[string]string `json:"target"`
}

func init() {
	application.RegisterEvent[TermDataEvent](EventTermData)
	application.RegisterEvent[TermExitEvent](EventTermExit)
	application.RegisterEvent[TransferEvent](EventTransfer)
	application.RegisterEvent[FileDropEvent](EventFileDrop)
}

func emit(name string, data any) { core.Emit(name, data) }

func sudoRun(ctx context.Context, conn *sshx.Conn, password, cmd string, stdin string) (sshx.ExecResult, error) {
	return core.SudoRun(ctx, conn, password, cmd, stdin)
}

func cmdError(res sshx.ExecResult) error { return core.CmdError(res) }

func firstNonEmpty(vals ...string) string { return core.FirstNonEmpty(vals...) }
