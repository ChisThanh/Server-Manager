package deploy

import (
	"strings"

	"server-manager/internal/apperr"
)

// Env file format
//
// The file is written as one `NAME=<quoted value>` per line, chosen so the
// same file is read identically, with no interpolation or escape processing,
// by:
//   - docker compose (`.env` interpolation file and `env_file:`),
//   - systemd `EnvironmentFile=`,
//   - POSIX sh (`set -a; . ./.env; set +a`), which the deploy steps use,
//   - common dotenv libraries (node dotenv, python-dotenv, godotenv).
//
// Values are wrapped in single quotes, where all of these readers take the
// content literally. A value containing a single quote is wrapped in double
// quotes instead, which is literal as long as it has none of `"`, `\`, `$`
// or “ ` “ (those would be escapes or interpolation in at least one reader).
// Values that cannot be represented this way — a mix of `'` and one of
// those characters, newlines, NUL and other control characters — are
// rejected with deploy.envValue; store multi-line data (PEM keys…)
// base64-encoded instead.

const envHeader = "# Managed by Server Manager — this file is rewritten on every deployment.\n# Edit the variables in the app instead of here.\n"

// quoteEnv returns the quoted form of v, or false if v can't be written.
func quoteEnv(v string) (string, bool) {
	for _, c := range v {
		if c == '\t' {
			continue
		}
		if c < 0x20 || c == 0x7f {
			return "", false
		}
	}
	if !strings.Contains(v, "'") {
		return "'" + v + "'", true
	}
	if strings.ContainsAny(v, "\"\\$`") {
		return "", false
	}
	return `"` + v + `"`, true
}

func checkEnvValue(name, v string) error {
	if len(v) > 32<<10 {
		return apperr.New("deploy.envValue", "name", name)
	}
	if _, ok := quoteEnv(v); !ok {
		return apperr.New("deploy.envValue", "name", name)
	}
	return nil
}

// renderEnv builds the env file: plain vars, then secrets, then extra
// (the image tag variable) — later entries override earlier ones.
func renderEnv(vars []EnvVar, secrets []EnvVar, extra []EnvVar) (string, error) {
	var b strings.Builder
	b.WriteString(envHeader)
	write := func(list []EnvVar) error {
		for _, v := range list {
			if !ValidEnvName(v.Name) {
				return apperr.New("deploy.invalidEnvName", "name", v.Name)
			}
			q, ok := quoteEnv(v.Value)
			if !ok {
				return apperr.New("deploy.envValue", "name", v.Name)
			}
			b.WriteString(v.Name)
			b.WriteByte('=')
			b.WriteString(q)
			b.WriteByte('\n')
		}
		return nil
	}
	for _, l := range [][]EnvVar{vars, secrets, extra} {
		if err := write(l); err != nil {
			return "", err
		}
	}
	return b.String(), nil
}

// parseEnvValue reads NAME's value from an env file written by renderEnv
// (or a simple hand-written one). Returns "" if absent.
func parseEnvValue(content, name string) string {
	val := ""
	for _, line := range strings.Split(content, "\n") {
		line = strings.TrimSpace(line)
		line = strings.TrimPrefix(line, "export ")
		k, v, ok := strings.Cut(line, "=")
		if !ok || strings.TrimSpace(k) != name {
			continue
		}
		v = strings.TrimSpace(v)
		if len(v) >= 2 && (v[0] == '\'' || v[0] == '"') && v[len(v)-1] == v[0] {
			v = v[1 : len(v)-1]
		} else if i := strings.Index(v, " #"); i >= 0 {
			v = strings.TrimSpace(v[:i])
		}
		val = v
	}
	return val
}
