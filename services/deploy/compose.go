package deploy

import (
	"encoding/json"
	"strings"

	"server-manager/internal/core"
)

const composeHeader = "# Managed by Server Manager — this file is regenerated on every deployment.\n# Edit the application in Server Manager instead of changing it here.\n"

// yq quotes s as a YAML scalar (JSON strings are valid YAML) and escapes
// "$" so docker compose doesn't interpolate it.
func yq(s string) string {
	b, _ := json.Marshal(strings.ReplaceAll(s, "$", "$$"))
	return string(b)
}

// renderCompose builds the compose file of a TypeImage app. The image tag
// comes from the tag variable in the env file, so a release only rewrites
// the env file.
func renderCompose(a *App) string {
	im := a.Image
	var b strings.Builder
	b.WriteString(composeHeader)
	b.WriteString("services:\n  app:\n")
	// The ${VAR:?} form makes compose fail loudly if the variable is missing.
	img, _ := json.Marshal(im.Image + ":${" + a.TagVar + ":?" + a.TagVar + " is not set}")
	b.WriteString("    image: " + string(img) + "\n")
	if im.ContainerName != "" {
		b.WriteString("    container_name: " + yq(im.ContainerName) + "\n")
	}
	b.WriteString("    restart: " + yq(im.Restart) + "\n")
	b.WriteString("    env_file:\n      - " + yq(a.envPath()) + "\n")
	if im.Command != "" {
		b.WriteString("    command: [\"sh\", \"-c\", " + yq(im.Command) + "]\n")
	}
	if len(im.Ports) > 0 {
		b.WriteString("    ports:\n")
		for _, p := range im.Ports {
			b.WriteString("      - " + yq(p) + "\n")
		}
	}
	if len(im.Volumes) > 0 {
		b.WriteString("    volumes:\n")
		for _, v := range im.Volumes {
			b.WriteString("      - " + yq(v) + "\n")
		}
	}
	if im.HealthCmd != "" {
		b.WriteString("    healthcheck:\n")
		b.WriteString("      test: [\"CMD-SHELL\", " + yq(im.HealthCmd) + "]\n")
		if im.HealthInterval > 0 {
			b.WriteString("      interval: " + itoa(im.HealthInterval) + "s\n")
		}
		if im.HealthTimeout > 0 {
			b.WriteString("      timeout: " + itoa(im.HealthTimeout) + "s\n")
		}
		if im.HealthRetries > 0 {
			b.WriteString("      retries: " + itoa(im.HealthRetries) + "\n")
		}
	}
	// Named volumes must be declared at the top level.
	named := []string{}
	for _, v := range im.Volumes {
		src := strings.SplitN(v, ":", 2)[0]
		if !strings.HasPrefix(src, "/") && !strings.HasPrefix(src, ".") {
			named = append(named, src)
		}
	}
	if len(named) > 0 {
		b.WriteString("volumes:\n")
		seen := map[string]bool{}
		for _, n := range named {
			if !seen[n] {
				seen[n] = true
				b.WriteString("  " + yq(n) + ": {}\n")
			}
		}
	}
	return b.String()
}

// composeCmd is the compose invocation for the app ("docker compose" or
// "docker-compose").
func composeCmd(bin string, a *App) string {
	s := bin + " -f " + core.Q(a.composePath()) + " --env-file " + core.Q(a.envPath())
	if a.Project != "" {
		s += " -p " + core.Q(a.Project)
	}
	return s
}
