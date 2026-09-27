// Package apperr defines coded errors. The frontend translates an error by
// its Code and Params, so user-facing text never lives in Go; Error() gives
// an English rendering for logs.
package apperr

import (
	"errors"
	"strings"
)

type Error struct {
	Code   string            `json:"code"`
	Params map[string]string `json:"params,omitempty"`
	// Detail carries the underlying technical message (e.g. command stderr),
	// shown verbatim after the translated text.
	Detail string `json:"detail,omitempty"`
	err    error
}

// New creates an error; params are key/value pairs.
func New(code string, params ...string) *Error {
	e := &Error{Code: code}
	if len(params) > 1 {
		e.Params = map[string]string{}
		for i := 0; i+1 < len(params); i += 2 {
			e.Params[params[i]] = params[i+1]
		}
	}
	return e
}

// Wrap creates an error whose Detail is err's message.
func Wrap(err error, code string, params ...string) *Error {
	e := New(code, params...)
	if err != nil {
		e.err = err
		e.Detail = err.Error()
	}
	return e
}

// WithDetail sets a free-form detail message.
func (e *Error) WithDetail(d string) *Error {
	e.Detail = strings.TrimSpace(d)
	return e
}

func (e *Error) Unwrap() error { return e.err }

func (e *Error) Is(target error) bool {
	t, ok := target.(*Error)
	return ok && t.Code == e.Code
}

func (e *Error) Error() string {
	msg, ok := english[e.Code]
	if !ok {
		msg = e.Code
	}
	for k, v := range e.Params {
		msg = strings.ReplaceAll(msg, "{"+k+"}", v)
	}
	if e.Detail != "" {
		if msg == "" {
			return e.Detail
		}
		return msg + ": " + e.Detail
	}
	return msg
}

// From returns err as an *Error, wrapping unknown errors as "generic".
func From(err error) *Error {
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return Wrap(err, "generic")
}

// HasCode reports whether err carries the given code.
func HasCode(err error, code string) bool {
	var e *Error
	return errors.As(err, &e) && e.Code == code
}

var english = map[string]string{
	"generic":                "",
	"server.notFound":        "server not found",
	"server.hostRequired":    "host is required",
	"server.userRequired":    "user is required",
	"keychain.saveFailed":    "could not save the password to the keychain",
	"conn.notConnected":      "not connected to this server",
	"conn.closed":            "connection closed",
	"conn.lost":              "connection lost",
	"conn.keepaliveFailed":   "keepalive failed",
	"conn.reconnecting":      "connection lost, reconnecting",
	"conn.sftpUnsupported":   "the server does not support the SFTP subsystem",
	"conn.dialFailed":        "cannot connect to {addr}",
	"auth.failed":            "authentication failed (wrong user/password/key)",
	"auth.needSecret":        "a password or passphrase is required",
	"auth.invalidType":       "invalid auth type {type}",
	"key.readFailed":         "cannot read private key {path}",
	"key.notFound":           "private key not found",
	"agent.notFound":         "ssh-agent not found (SSH_AUTH_SOCK is empty)",
	"agent.dialFailed":       "cannot connect to ssh-agent",
	"term.closed":            "terminal closed",
	"term.idle":              "closed after {min} minutes without activity",
	"term.recordingNotFound": "recording not found",
	"term.recordingTooLarge": "recording is too large to replay",
	"term.invalidTarget":     "invalid console target {target}",
	"sys.invalidPid":         "invalid PID",
	"sys.noSystemd":          "the server does not use systemd",
	"sys.invalidAction":      "invalid action",
	"sys.invalidUnit":        "invalid service name",
	"cmd.failed":             "command exited with code {code}",
	"sudo.required":          "sudo password required",
	"sudo.wrongPassword":     "wrong sudo password",
	"sudo.failed":            "sudo command failed",
	"sudo.missing":           "sudo is not installed",
	"sudo.notAllowed":        "this account may not use sudo",
	"access.denied":          "the {role} role may not do this ({perm})",
	"fs.isDir":               "{path} is a directory",
	"fs.writeFailedBackup":   "write failed; the full content is still in {tmp}",
	"fs.tooManyLinks":        "too many nested symlinks",
	"fs.exists":              "{name} already exists",
	"fs.moveIntoSelf":        "cannot move {path} into itself",
	"fs.refuseRoot":          "refusing to delete the root directory",
	"fs.noSelection":         "nothing selected",
	"fs.archiveUnsupported":  "unsupported archive format",
	"fs.permission":          "permission denied: {path}",
	"fs.notFound":            "not found: {path}",
	"fs.noFiles":             "no files to upload",
	"fs.mkdirFailed":         "cannot create directory {path}",
	"fs.invalidOwner":        "invalid owner or group",
	"fs.invalidOp":           "invalid operation",
}
