package docker

import (
	"path"
	"regexp"
	"strconv"
	"strings"

	"server-manager/internal/apperr"
)

// Strict formats for everything interpolated into docker commands. Values
// are also quoted with core.Q; the checks turn bad input into clear errors
// and keep option-looking values ("-rf") away from the CLI.
var (
	// Container / volume / network names and ids (docker's own rule).
	objectRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,254}$`)
	// Compose project names (compose normalizes to this).
	projectRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,62}$`)
	// Compose service names.
	serviceRe = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,62}$`)
	// Image references: [registry[:port]/]path[:tag][@sha256:digest].
	imageRefRe = regexp.MustCompile(`^(?:[a-zA-Z0-9](?:[a-zA-Z0-9.-]*[a-zA-Z0-9])?(?::[0-9]{1,5})?/)?` +
		`[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*(?:/[a-z0-9]+(?:(?:[._]|__|-+)[a-z0-9]+)*)*` +
		`(?::[a-zA-Z0-9_][a-zA-Z0-9_.-]{0,127})?(?:@sha256:[a-f0-9]{64})?$`)
	// Image ids (full or short).
	imageIDRe = regexp.MustCompile(`^(?:sha256:)?[a-f0-9]{12,64}$`)
	// Absolute paths of compose files / project directories. No commas
	// (compose joins config files with commas in its labels), no control
	// characters, no quotes.
	pathRe = regexp.MustCompile(`^/[A-Za-z0-9._@+=:~ /-]*$`)
	// --since: relative duration or a date/time.
	sinceRe = regexp.MustCompile(`^(?:[0-9]{1,6}(?:s|m|h)|[0-9]{9,11}|[0-9]{4}-[0-9]{2}-[0-9]{2}(?:T[0-9]{2}:[0-9]{2}(?::[0-9]{2})?(?:Z|[+-][0-9]{2}:[0-9]{2})?)?)$`)
)

func checkObject(kind, name string) error {
	if !objectRe.MatchString(name) {
		return apperr.New("docker.invalidName", "kind", kind, "name", name)
	}
	return nil
}

func checkProject(name string) error {
	if !projectRe.MatchString(name) {
		return apperr.New("docker.invalidName", "kind", "project", "name", name)
	}
	return nil
}

func checkService(name string) error {
	if !serviceRe.MatchString(name) {
		return apperr.New("docker.invalidName", "kind", "service", "name", name)
	}
	return nil
}

func checkImage(ref string) error {
	if len(ref) > 512 || !(imageRefRe.MatchString(ref) || imageIDRe.MatchString(ref)) {
		return apperr.New("docker.invalidName", "kind", "image", "name", ref)
	}
	return nil
}

// checkPath validates an absolute path and returns it cleaned.
func checkPath(p string) (string, error) {
	if len(p) > 1024 || !pathRe.MatchString(p) {
		return "", apperr.New("docker.invalidPath", "path", p)
	}
	for _, seg := range strings.Split(p, "/") {
		if seg == ".." || strings.HasPrefix(seg, "-") {
			return "", apperr.New("docker.invalidPath", "path", p)
		}
	}
	c := path.Clean(p)
	if c == "/" {
		return "", apperr.New("docker.invalidPath", "path", p)
	}
	return c, nil
}

func checkPaths(ps []string) ([]string, error) {
	out := make([]string, 0, len(ps))
	for _, p := range ps {
		c, err := checkPath(p)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, nil
}

func itoa(n int) string { return strconv.Itoa(n) }
