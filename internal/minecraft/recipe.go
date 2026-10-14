package minecraft

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"time"
)

type RecipeStep struct {
	Type string            `json:"type"`
	With map[string]string `json:"with"`
}

func quote(value string) string { return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'" }

func recipePath(value string, root bool) (string, error) {
	if root && value == "." {
		return value, nil
	}
	_, err := SafePath(value)
	return value, err
}

func stepScript(step RecipeStep) (string, error) {
	get := func(key string) string { return step.With[key] }
	path := func(key string, root bool) (string, error) {
		value, err := recipePath(get(key), root)
		return quote(value), err
	}
	switch step.Type {
	case "git":
		remote, err := url.Parse(get("repository"))
		if err != nil || remote.Scheme != "https" || remote.Host == "" || remote.User != nil || strings.ContainsAny(remote.String(), "\r\n") {
			return "", errors.New("Git requires a public HTTPS repository URL")
		}
		target, err := path("target", false)
		if err != nil {
			return "", err
		}
		ref := get("ref")
		if ref == "" || strings.HasPrefix(ref, "-") || len(ref) > 128 || strings.ContainsAny(ref, "\r\n") {
			return "", errors.New("Git requires an explicit tag or commit")
		}
		refArg := quote(ref + "^{commit}")
		if match := regexp.MustCompile(`^\{\{([A-Za-z_][A-Za-z0-9_]*)\}\}$`).FindStringSubmatch(ref); match != nil {
			refArg = "\"${" + match[1] + "}^{commit}\""
		}
		return "git clone --no-checkout -- " + quote(remote.String()) + " " + target + "\ngit -C " + target + " checkout --detach \"$(git -C " + target + " rev-parse --verify --end-of-options " + refArg + ")\"\n", nil
	case "mkdir", "delete":
		target, err := path("path", false)
		if err != nil {
			return "", err
		}
		if step.Type == "mkdir" {
			return "mkdir -p -- " + target + "\n", nil
		}
		return "rm -rf -- " + target + "\n", nil
	case "copy", "move":
		source, err := path("source", false)
		if err != nil {
			return "", err
		}
		target, err := path("target", true)
		if err != nil {
			return "", err
		}
		if step.Type == "copy" {
			return "if [ -d " + source + " ]; then mkdir -p -- " + target + "; cp -a -- " + source + "/. " + target + "/; else mkdir -p -- \"$(dirname " + target + ")\"; cp -a -- " + source + " " + target + "; fi\n", nil
		}
		return "mkdir -p -- \"$(dirname " + target + ")\"\nmv -- " + source + " " + target + "\n", nil
	case "write_file":
		target, err := path("path", false)
		if err != nil {
			return "", err
		}
		return "mkdir -p -- \"$(dirname " + target + ")\"\nprintf '%s' " + quote(get("content")) + " > " + target + "\n", nil
	case "download":
		remote, err := url.Parse(get("url"))
		if err != nil || remote.Scheme != "https" || remote.Host == "" || remote.User != nil {
			return "", errors.New("Download requires HTTPS")
		}
		target, err := path("path", false)
		if err != nil {
			return "", err
		}
		return "mkdir -p -- \"$(dirname " + target + ")\"\ncurl --fail --location --proto '=https' --proto-redir '=https' --max-time 300 --output " + target + " " + quote(remote.String()) + "\n", nil
	case "extract":
		source, err := path("source", false)
		if err != nil {
			return "", err
		}
		target, err := path("target", true)
		if err != nil {
			return "", err
		}
		if get("format") != "tar.gz" {
			return "", errors.New("Recipe extraction currently supports tar.gz")
		}
		return "mkdir -p -- " + target + "\ntar --extract --gzip --no-same-owner --no-same-permissions --file " + source + " --directory " + target + "\n", nil
	case "run":
		directory, err := path("directory", true)
		if err != nil {
			return "", err
		}
		command := get("command")
		if command == "" || len(command) > 8192 {
			return "", errors.New("Recipe command missing or too long")
		}
		// Commands are trusted administrator-authored scripts. User values are environment variables, never interpolated into shell source.
		return "(cd " + directory + "\n" + command + "\n)\n", nil
	}
	return "", errors.New("unsupported recipe step")
}

func runRecipe(ctx context.Context, stage, image string, steps []RecipeStep, variables map[string]string, uid, gid int, log func(string)) error {
	if len(steps) == 0 {
		return nil
	}
	var script strings.Builder
	script.WriteString("set -eu\numask 027\n")
	for index, step := range steps {
		body, err := stepScript(step)
		if err != nil {
			return fmt.Errorf("recipe step %d: %w", index+1, err)
		}
		fmt.Fprintf(&script, "printf 'Step %d/%d: %s\\n'\n", index+1, len(steps), step.Type)
		script.WriteString(body)
	}
	name := "hivepanel-recipe-" + filepath.Base(stage)
	args := []string{"run", "--rm", "--name", name, "--user", fmt.Sprintf("%d:%d", uid, gid), "--cap-drop", "ALL", "--security-opt", "no-new-privileges:true", "--read-only", "--tmpfs", "/tmp:rw,size=1g", "--pids-limit", "256", "--memory", "2g", "--cpus", "2", "-v", stage + ":/home/container:Z", "-w", "/home/container", "--env", "HOME=/home/container", "--entrypoint", "sh"}
	// A private env file keeps build values out of process arguments and prevents them from changing Docker CLI connection settings.
	var environmentFile strings.Builder
	for key, value := range variables {
		if !environmentName.MatchString(key) || strings.ContainsAny(value, "\r\n") {
			return errors.New("invalid installation environment value")
		}
		fmt.Fprintf(&environmentFile, "%s=%s\n", key, value)
	}
	envPath := filepath.Join(stage, ".recipe-env")
	if err := os.WriteFile(envPath, []byte(environmentFile.String()), 0600); err != nil {
		return err
	}
	defer os.Remove(envPath)
	args = append(args, "--env-file", envPath)
	args = append(args, image, "-ec", script.String())
	if err := os.WriteFile(filepath.Join(stage, ".recipe-container"), []byte(name), 0600); err != nil {
		return err
	}
	defer os.Remove(filepath.Join(stage, ".recipe-container"))
	command := exec.CommandContext(ctx, "docker", args...)

	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		_ = exec.CommandContext(cleanup, "docker", "rm", "-f", name).Run()
	}()
	secrets := []string{}
	if data, err := os.ReadFile(filepath.Join(stage, ".env")); err == nil {
		for _, line := range strings.Split(string(data), "\n") {
			parts := strings.SplitN(line, "=", 2)
			if len(parts) == 2 && len(parts[1]) > 5 {
				secrets = append(secrets, parts[1])
			}
		}
	}
	return streamCommand(command, func(line string) {
		for _, secret := range secrets {
			line = strings.ReplaceAll(line, secret, "[redacted]")
		}
		log(line)
	})
}

func safeStagedLink(stage, path string) error {
	link, err := os.Readlink(path)
	if err != nil {
		return err
	}
	if filepath.IsAbs(link) {
		return errors.New("installer produced an absolute symlink")
	}
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return err
	}
	relative, err := filepath.Rel(stage, resolved)
	if err != nil {
		return err
	}
	if _, err := SafePath(filepath.ToSlash(relative)); err != nil {
		return errors.New("installer produced a symlink outside its staging tree")
	}
	return nil
}

func recoverRecipeContainers(root string) error {
	directory := filepath.Join(root, ".hivepanel", "content-staging")
	entries, err := os.ReadDir(directory)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.IsDir() || !strings.HasPrefix(entry.Name(), "install-") {
			continue
		}
		stage := filepath.Join(directory, entry.Name())
		marker, err := os.ReadFile(filepath.Join(stage, ".recipe-container"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		name := "hivepanel-recipe-" + entry.Name()
		if string(marker) != name {
			return errors.New("invalid recipe recovery marker")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		output, removeErr := exec.CommandContext(ctx, "docker", "rm", "-f", name).CombinedOutput()
		cancel()
		if removeErr != nil && !strings.Contains(string(output), "No such container") {
			return fmt.Errorf("recipe container recovery failed: %w", removeErr)
		}
		if err := os.RemoveAll(stage); err != nil {
			return err
		}
	}
	return nil
}
