package minecraft

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

var imageReference = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_:@-]{1,250}$`)
var environmentName = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

func validatePreinstallRuntime(runtime *PreinstallRuntime) error {
	if !imageReference.MatchString(runtime.Image) || runtime.Startup == "" || len(runtime.Startup) > 4096 || len(runtime.SecretKeys) > 10 || len(runtime.Environment) > 100 || len(runtime.VariablesSchema) > 50 || len(runtime.Steps) > 100 {
		return errors.New("invalid preinstall runtime")
	}
	for key, value := range runtime.Environment {
		if !environmentName.MatchString(key) || len(value) > 4096 {
			return errors.New("invalid preinstall environment")
		}
	}
	for key, value := range runtime.Parameters {
		if len(value) > 4096 {
			return errors.New("installation variable too long")
		}
		allowed := false
		for _, variable := range runtime.VariablesSchema {
			if variable["name"] == key && variable["install"] == true && variable["secret"] != true {
				allowed = true
			}
		}
		if !allowed {
			return errors.New("undeclared or private installation variable")
		}
	}
	for _, key := range runtime.SecretKeys {
		if !environmentName.MatchString(key) {
			return errors.New("invalid generated secret name")
		}
	}
	for _, variable := range runtime.VariablesSchema {
		name, _ := variable["name"].(string)
		if !environmentName.MatchString(name) {
			return errors.New("invalid preinstall variable")
		}
	}
	for _, step := range runtime.Steps {
		if _, err := stepScript(step); err != nil {
			return err
		}
	}
	return nil
}

func writePreinstallSecrets(stage string, keys []string) error {
	if len(keys) == 0 {
		return nil
	}
	// Secrets are generated on the Worker, never returned in previews or job JSON.
	var output strings.Builder
	for _, key := range keys {
		value := make([]byte, 32)
		if _, err := rand.Read(value); err != nil {
			return err
		}
		fmt.Fprintf(&output, "%s=%s\n", key, hex.EncodeToString(value))
	}
	file, err := os.OpenFile(filepath.Join(stage, ".env"), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	_, err = file.WriteString(output.String())
	closeErr := file.Close()
	if err != nil {
		return err
	}
	return closeErr
}

func streamCommand(command *exec.Cmd, log func(string)) error {
	output, err := command.StdoutPipe()
	if err != nil {
		return err
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		return err
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		log(scanner.Text())
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if scanErr != nil {
		return scanErr
	}
	return waitErr
}

func preparePreinstall(ctx context.Context, stage string, runtime *PreinstallRuntime, log func(string)) (string, error) {
	if err := validatePreinstallRuntime(runtime); err != nil {
		return "", err
	}
	log("Downloading the preinstall’s runtime image and dependencies…")
	command := exec.CommandContext(ctx, "docker", "pull", runtime.Image)
	output, err := command.StdoutPipe()
	if err != nil {
		return "", err
	}
	command.Stderr = command.Stdout
	if err := command.Start(); err != nil {
		return "", err
	}
	scanner := bufio.NewScanner(output)
	scanner.Buffer(make([]byte, 4096), 1024*1024)
	for scanner.Scan() {
		log(scanner.Text())
	}
	scanErr := scanner.Err()
	if scanErr != nil {
		_ = command.Process.Kill()
	}
	waitErr := command.Wait()
	if scanErr != nil {
		return "", scanErr
	}
	if waitErr != nil {
		return "", fmt.Errorf("preinstall image download failed: %w", waitErr)
	}
	log("Preparing private Cell configuration…")
	if err := writePreinstallSecrets(stage, runtime.SecretKeys); err != nil {
		return "", err
	}
	digest, err := exec.CommandContext(ctx, "docker", "image", "inspect", "--format", "{{index .RepoDigests 0}}", runtime.Image).Output()
	if err != nil {
		return "", fmt.Errorf("could not pin preinstall image: %w", err)
	}
	pinned := strings.TrimSpace(string(digest))
	if !regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9./_-]+@sha256:[a-f0-9]{64}$`).MatchString(pinned) {
		return "", errors.New("image has no verified repository digest")
	}
	return pinned, nil
}
