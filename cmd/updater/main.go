package main

import (
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

const (
	defaultBinaryPath = "/usr/local/bin/hiveworker"
	defaultService    = "hiveworker"
	defaultHealthURL  = "http://127.0.0.1:8080/health"

	waitForExitTimeout = 30 * time.Second
	healthTimeout      = 60 * time.Second
	healthInterval     = 2 * time.Second
)

func main() {
	var (
		pid        int
		stagedPath string
		target     string
		binaryPath string
		service    string
		healthURL  string
	)

	flag.IntVar(
		&pid,
		"pid",
		0,
		"PID of the currently running HiveWorker process",
	)

	flag.StringVar(
		&stagedPath,
		"staged",
		"",
		"path to the staged HiveWorker binary",
	)

	flag.StringVar(
		&target,
		"target",
		"",
		"target HiveWorker version",
	)

	flag.StringVar(
		&binaryPath,
		"binary",
		defaultBinaryPath,
		"path to the installed HiveWorker binary",
	)

	flag.StringVar(
		&service,
		"service",
		defaultService,
		"systemd service name",
	)

	flag.StringVar(
		&healthURL,
		"health-url",
		defaultHealthURL,
		"HiveWorker health endpoint",
	)

	flag.Parse()

	if pid <= 0 {
		fatal("invalid or missing --pid")
	}

	stagedPath = strings.TrimSpace(stagedPath)
	if stagedPath == "" {
		fatal("missing --staged")
	}

	target = strings.TrimSpace(target)
	if target == "" {
		fatal("missing --target")
	}

	if os.Geteuid() != 0 {
		fatal("updater must run as root")
	}

	if err := validateStagedBinary(stagedPath); err != nil {
		fatal("staged binary validation failed: %v", err)
	}

	backupPath := binaryPath + ".backup"

	logf(
		"starting HiveWorker update to %s; waiting for PID %d to exit",
		target,
		pid,
	)

	if err := waitForProcessExit(pid, waitForExitTimeout); err != nil {
		fatal("worker did not exit: %v", err)
	}

	logf("HiveWorker stopped; installing staged binary")

	if err := removeIfExists(backupPath); err != nil {
		fatal("could not remove old backup: %v", err)
	}

	if err := copyFile(binaryPath, backupPath, 0755); err != nil {
		fatal("could not back up current worker binary: %v", err)
	}

	if err := installBinary(stagedPath, binaryPath); err != nil {
		logf("failed to install new binary: %v", err)

		if rollbackErr := rollback(
			backupPath,
			binaryPath,
			service,
			healthURL,
		); rollbackErr != nil {
			fatal(
				"installation failed and rollback also failed: install=%v rollback=%v",
				err,
				rollbackErr,
			)
		}

		fatal("installation failed; previous version restored: %v", err)
	}

	logf("new binary installed; starting %s", service)

	if err := systemctl("start", service); err != nil {
		logf("new worker failed to start: %v", err)

		if rollbackErr := rollback(
			backupPath,
			binaryPath,
			service,
			healthURL,
		); rollbackErr != nil {
			fatal(
				"new worker failed to start and rollback failed: start=%v rollback=%v",
				err,
				rollbackErr,
			)
		}

		fatal("new worker failed to start; previous version restored")
	}

	logf("waiting for HiveWorker health check")

	if err := waitForHealth(healthURL, healthTimeout); err != nil {
		logf("new worker failed health check: %v", err)

		if rollbackErr := rollback(
			backupPath,
			binaryPath,
			service,
			healthURL,
		); rollbackErr != nil {
			fatal(
				"health check failed and rollback failed: health=%v rollback=%v",
				err,
				rollbackErr,
			)
		}

		fatal("new worker failed health check; previous version restored")
	}

	if err := removeIfExists(backupPath); err != nil {
		logf(
			"warning: update succeeded but backup could not be removed: %v",
			err,
		)
	}

	if err := removeIfExists(stagedPath); err != nil {
		logf(
			"warning: update succeeded but staged binary could not be removed: %v",
			err,
		)
	}

	logf("HiveWorker successfully updated to %s", target)
}

func validateStagedBinary(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf("staged path is not a regular file")
	}

	if info.Size() == 0 {
		return fmt.Errorf("staged binary is empty")
	}

	if err := os.Chmod(path, 0755); err != nil {
		return fmt.Errorf("chmod staged binary: %w", err)
	}

	/*
		Do not execute the staged Worker here.

		The Worker currently requires its normal runtime/configuration
		environment and does not expose a reliable --version flag.

		SHA-256 verification should already have been performed by
		internal/updater before this helper is launched.
	*/
	return nil
}

func waitForProcessExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)

	for {
		running, err := processExists(pid)
		if err != nil {
			return err
		}

		if !running {
			return nil
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"process %d still running after %s",
				pid,
				timeout,
			)
		}

		time.Sleep(250 * time.Millisecond)
	}
}

func processExists(pid int) (bool, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}

	err = process.Signal(syscall.Signal(0))

	if err == nil {
		return true, nil
	}

	if err == os.ErrProcessDone {
		return false, nil
	}

	if errno, ok := err.(syscall.Errno); ok {
		if errno == syscall.ESRCH {
			return false, nil
		}

		if errno == syscall.EPERM {
			return true, nil
		}
	}

	return false, err
}

func installBinary(source, destination string) error {
	directory := filepath.Dir(destination)

	tempFile, err := os.CreateTemp(
		directory,
		".hiveworker-update-*",
	)
	if err != nil {
		return fmt.Errorf("create temporary binary: %w", err)
	}

	tempPath := tempFile.Name()
	success := false

	defer func() {
		_ = tempFile.Close()

		if !success {
			_ = os.Remove(tempPath)
		}
	}()

	sourceFile, err := os.Open(source)
	if err != nil {
		return fmt.Errorf("open staged binary: %w", err)
	}
	defer sourceFile.Close()

	if _, err := io.Copy(tempFile, sourceFile); err != nil {
		return fmt.Errorf("copy staged binary: %w", err)
	}

	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf("sync temporary binary: %w", err)
	}

	if err := tempFile.Chmod(0755); err != nil {
		return fmt.Errorf("chmod temporary binary: %w", err)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf("close temporary binary: %w", err)
	}

	if err := os.Rename(tempPath, destination); err != nil {
		return fmt.Errorf("replace worker binary: %w", err)
	}

	success = true

	if err := syncDirectory(directory); err != nil {
		return fmt.Errorf("sync worker directory: %w", err)
	}

	return nil
}

func rollback(
	backupPath string,
	binaryPath string,
	service string,
	healthURL string,
) error {
	logf("rolling back to previous HiveWorker binary")

	/*
		Ensure a partially started/broken Worker isn't still running before
		we replace its binary.
	*/
	_ = systemctl("stop", service)

	if _, err := os.Stat(backupPath); err != nil {
		return fmt.Errorf("backup binary unavailable: %w", err)
	}

	if err := installBinary(backupPath, binaryPath); err != nil {
		return fmt.Errorf("restore backup binary: %w", err)
	}

	if err := systemctl("start", service); err != nil {
		return fmt.Errorf("restart previous worker: %w", err)
	}

	if err := waitForHealth(healthURL, healthTimeout); err != nil {
		return fmt.Errorf(
			"previous worker did not become healthy: %w",
			err,
		)
	}

	if err := removeIfExists(backupPath); err != nil {
		logf(
			"warning: rollback succeeded but backup could not be removed: %v",
			err,
		)
	}

	logf("rollback completed successfully")

	return nil
}

func waitForHealth(url string, timeout time.Duration) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
	}

	deadline := time.Now().Add(timeout)

	for {
		request, err := http.NewRequest(
			http.MethodGet,
			url,
			nil,
		)
		if err != nil {
			return err
		}

		response, err := client.Do(request)

		if err == nil {
			_, _ = io.Copy(io.Discard, response.Body)
			_ = response.Body.Close()

			if response.StatusCode >= 200 &&
				response.StatusCode < 300 {
				return nil
			}
		}

		if time.Now().After(deadline) {
			return fmt.Errorf(
				"health endpoint %s did not become healthy within %s",
				url,
				timeout,
			)
		}

		time.Sleep(healthInterval)
	}
}

func systemctl(action, service string) error {
	if strings.TrimSpace(service) == "" {
		return fmt.Errorf("systemd service name is empty")
	}

	cmd := exec.Command(
		"systemctl",
		action,
		service,
	)

	output, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"systemctl %s %s failed: %w: %s",
			action,
			service,
			err,
			strings.TrimSpace(string(output)),
		)
	}

	return nil
}

func copyFile(
	source string,
	destination string,
	mode os.FileMode,
) error {
	sourceFile, err := os.Open(source)
	if err != nil {
		return err
	}
	defer sourceFile.Close()

	info, err := sourceFile.Stat()
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf(
			"%s is not a regular file",
			source,
		)
	}

	destinationFile, err := os.OpenFile(
		destination,
		os.O_WRONLY|os.O_CREATE|os.O_TRUNC,
		mode,
	)
	if err != nil {
		return err
	}

	success := false

	defer func() {
		_ = destinationFile.Close()

		if !success {
			_ = os.Remove(destination)
		}
	}()

	if _, err := io.Copy(
		destinationFile,
		sourceFile,
	); err != nil {
		return err
	}

	if err := destinationFile.Sync(); err != nil {
		return err
	}

	if err := destinationFile.Close(); err != nil {
		return err
	}

	if err := os.Chmod(destination, mode); err != nil {
		return err
	}

	success = true

	return nil
}

func removeIfExists(path string) error {
	err := os.Remove(path)

	if err == nil || os.IsNotExist(err) {
		return nil
	}

	return err
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()

	return directory.Sync()
}

func logf(format string, args ...any) {
	message := fmt.Sprintf(format, args...)

	fmt.Fprintf(
		os.Stderr,
		"[hiveworker-updater] %s %s\n",
		time.Now().UTC().Format(time.RFC3339),
		message,
	)
}

func fatal(format string, args ...any) {
	logf("ERROR: "+format, args...)
	os.Exit(1)
}
