package main

import (
	"crypto/tls"
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

	exitTimeout     = 30 * time.Second
	restartTimeout  = 60 * time.Second
	healthTimeout   = 60 * time.Second
	healthInterval  = 2 * time.Second
	processInterval = 250 * time.Millisecond
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
		"",
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

	healthURL = strings.TrimSpace(healthURL)
	if healthURL == "" {
		fatal("missing --health-url")
	}

	if os.Geteuid() != 0 {
		fatal("updater must run as root")
	}

	if err := validateStagedBinary(
		stagedPath,
	); err != nil {
		fatal(
			"staged binary validation failed: %v",
			err,
		)
	}

	backupPath := binaryPath + ".backup"

	logf(
		"starting HiveWorker update to %s from PID %d",
		target,
		pid,
	)

	if err := removeIfExists(
		backupPath,
	); err != nil {
		fatal(
			"could not remove old backup: %v",
			err,
		)
	}

	logf("backing up current HiveWorker binary")

	if err := copyFile(
		binaryPath,
		backupPath,
		0755,
	); err != nil {
		fatal(
			"could not back up current Worker binary: %v",
			err,
		)
	}

	/*
		Atomically replace the path while the current Worker
		is still running.

		Linux keeps the old executable inode available to the
		running process. The next systemd start therefore gets
		the new binary.
	*/
	logf("installing staged HiveWorker binary")

	if err := installBinary(
		stagedPath,
		binaryPath,
	); err != nil {
		logf(
			"failed to install new binary: %v",
			err,
		)

		if rollbackErr := restoreBinary(
			backupPath,
			binaryPath,
		); rollbackErr != nil {
			fatal(
				"installation failed and backup restoration also failed: install=%v restore=%v",
				err,
				rollbackErr,
			)
		}

		fatal(
			"installation failed; previous binary restored: %v",
			err,
		)
	}

	logf(
		"new binary installed; terminating old Worker PID %d",
		pid,
	)

	if err := terminateProcess(
		pid,
		exitTimeout,
	); err != nil {
		logf(
			"old Worker did not terminate cleanly: %v",
			err,
		)

		if rollbackErr := rollback(
			backupPath,
			binaryPath,
			service,
			healthURL,
		); rollbackErr != nil {
			fatal(
				"Worker termination failed and rollback failed: terminate=%v rollback=%v",
				err,
				rollbackErr,
			)
		}

		fatal(
			"Worker termination failed; previous version restored",
		)
	}

	logf(
		"old Worker stopped; waiting for systemd restart",
	)

	newPID, err := waitForNewMainPID(
		service,
		pid,
		restartTimeout,
	)

	if err != nil {
		logf(
			"new Worker process did not appear: %v",
			err,
		)

		if rollbackErr := rollback(
			backupPath,
			binaryPath,
			service,
			healthURL,
		); rollbackErr != nil {
			fatal(
				"new Worker did not start and rollback failed: start=%v rollback=%v",
				err,
				rollbackErr,
			)
		}

		fatal(
			"new Worker did not start; previous version restored",
		)
	}

	logf(
		"new HiveWorker process started with PID %d",
		newPID,
	)

	logf(
		"waiting for HiveWorker health check at %s",
		healthURL,
	)

	if err := waitForHealth(
		healthURL,
		healthTimeout,
	); err != nil {
		logf(
			"new Worker failed health check: %v",
			err,
		)

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

		fatal(
			"new Worker failed health check; previous version restored",
		)
	}

	if err := removeIfExists(
		backupPath,
	); err != nil {
		logf(
			"warning: update succeeded but backup could not be removed: %v",
			err,
		)
	}

	if err := removeIfExists(
		stagedPath,
	); err != nil {
		logf(
			"warning: update succeeded but staged binary could not be removed: %v",
			err,
		)
	}

	logf(
		"HiveWorker successfully updated to %s",
		target,
	)
}

func validateStagedBinary(
	path string,
) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if !info.Mode().IsRegular() {
		return fmt.Errorf(
			"staged path is not a regular file",
		)
	}

	if info.Size() == 0 {
		return fmt.Errorf(
			"staged binary is empty",
		)
	}

	if err := os.Chmod(
		path,
		0755,
	); err != nil {
		return fmt.Errorf(
			"chmod staged binary: %w",
			err,
		)
	}

	return nil
}

func terminateProcess(
	pid int,
	timeout time.Duration,
) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}

	if err := process.Signal(
		syscall.SIGTERM,
	); err != nil {
		if err == os.ErrProcessDone {
			return nil
		}

		if errno, ok := err.(syscall.Errno); ok &&
			errno == syscall.ESRCH {
			return nil
		}

		return err
	}

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
				"process %d did not exit after %s",
				pid,
				timeout,
			)
		}

		time.Sleep(processInterval)
	}
}

func processExists(
	pid int,
) (bool, error) {
	process, err := os.FindProcess(pid)
	if err != nil {
		return false, err
	}

	err = process.Signal(
		syscall.Signal(0),
	)

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

func waitForNewMainPID(
	service string,
	oldPID int,
	timeout time.Duration,
) (int, error) {
	deadline := time.Now().Add(timeout)

	for {
		pid, err := systemdMainPID(service)

		if err == nil &&
			pid > 0 &&
			pid != oldPID {
			return pid, nil
		}

		if time.Now().After(deadline) {
			return 0, fmt.Errorf(
				"systemd did not start a new %s process within %s",
				service,
				timeout,
			)
		}

		time.Sleep(processInterval)
	}
}

func systemdMainPID(
	service string,
) (int, error) {
	service = strings.TrimSpace(service)

	if service == "" {
		return 0, fmt.Errorf(
			"systemd service name is empty",
		)
	}

	command := exec.Command(
		"systemctl",
		"show",
		service,
		"--property=MainPID",
		"--value",
	)

	output, err := command.CombinedOutput()
	if err != nil {
		return 0, fmt.Errorf(
			"systemctl show %s failed: %w: %s",
			service,
			err,
			strings.TrimSpace(
				string(output),
			),
		)
	}

	value := strings.TrimSpace(
		string(output),
	)

	if value == "" || value == "0" {
		return 0, nil
	}

	var pid int

	if _, err := fmt.Sscanf(
		value,
		"%d",
		&pid,
	); err != nil {
		return 0, fmt.Errorf(
			"invalid MainPID %q: %w",
			value,
			err,
		)
	}

	return pid, nil
}

func installBinary(
	source string,
	destination string,
) error {
	directory := filepath.Dir(destination)

	tempFile, err := os.CreateTemp(
		directory,
		".hiveworker-update-*",
	)
	if err != nil {
		return fmt.Errorf(
			"create temporary binary: %w",
			err,
		)
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
		return fmt.Errorf(
			"open staged binary: %w",
			err,
		)
	}
	defer sourceFile.Close()

	if _, err := io.Copy(
		tempFile,
		sourceFile,
	); err != nil {
		return fmt.Errorf(
			"copy staged binary: %w",
			err,
		)
	}

	if err := tempFile.Sync(); err != nil {
		return fmt.Errorf(
			"sync temporary binary: %w",
			err,
		)
	}

	if err := tempFile.Chmod(
		0755,
	); err != nil {
		return fmt.Errorf(
			"chmod temporary binary: %w",
			err,
		)
	}

	if err := tempFile.Close(); err != nil {
		return fmt.Errorf(
			"close temporary binary: %w",
			err,
		)
	}

	if err := os.Rename(
		tempPath,
		destination,
	); err != nil {
		return fmt.Errorf(
			"replace Worker binary: %w",
			err,
		)
	}

	success = true

	if err := syncDirectory(
		directory,
	); err != nil {
		return fmt.Errorf(
			"sync Worker directory: %w",
			err,
		)
	}

	return nil
}

func restoreBinary(
	backupPath string,
	binaryPath string,
) error {
	if _, err := os.Stat(
		backupPath,
	); err != nil {
		return fmt.Errorf(
			"backup binary unavailable: %w",
			err,
		)
	}

	if err := installBinary(
		backupPath,
		binaryPath,
	); err != nil {
		return fmt.Errorf(
			"restore backup binary: %w",
			err,
		)
	}

	return nil
}

func rollback(
	backupPath string,
	binaryPath string,
	service string,
	healthURL string,
) error {
	logf(
		"rolling back to previous HiveWorker binary",
	)

	/*
		At this stage the replacement Worker may already be
		running. Stop it before restoring the old executable.

		This helper may survive the Worker restart because the
		service itself has not been explicitly stopped during
		the normal update path.
	*/
	_ = systemctl(
		"stop",
		service,
	)

	if err := restoreBinary(
		backupPath,
		binaryPath,
	); err != nil {
		return err
	}

	if err := systemctl(
		"start",
		service,
	); err != nil {
		return fmt.Errorf(
			"restart previous Worker: %w",
			err,
		)
	}

	if err := waitForHealth(
		healthURL,
		healthTimeout,
	); err != nil {
		return fmt.Errorf(
			"previous Worker did not become healthy: %w",
			err,
		)
	}

	if err := removeIfExists(
		backupPath,
	); err != nil {
		logf(
			"warning: rollback succeeded but backup could not be removed: %v",
			err,
		)
	}

	logf(
		"rollback completed successfully",
	)

	return nil
}

func waitForHealth(
	url string,
	timeout time.Duration,
) error {
	client := &http.Client{
		Timeout: 3 * time.Second,
	}

	if strings.HasPrefix(
		strings.ToLower(url),
		"https://",
	) {
		// The updater checks the Worker over loopback. Native TLS
		// certificates are issued for the public Worker hostname, not
		// 127.0.0.1, so hostname verification is intentionally skipped
		// for this local health probe only.
		client.Transport = &http.Transport{
			TLSClientConfig: &tls.Config{
				InsecureSkipVerify: true, // #nosec G402 -- loopback health check only
			},
		}
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
			_, _ = io.Copy(
				io.Discard,
				response.Body,
			)

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

func systemctl(
	action string,
	service string,
) error {
	service = strings.TrimSpace(service)

	if service == "" {
		return fmt.Errorf(
			"systemd service name is empty",
		)
	}

	command := exec.Command(
		"systemctl",
		action,
		service,
	)

	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"systemctl %s %s failed: %w: %s",
			action,
			service,
			err,
			strings.TrimSpace(
				string(output),
			),
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
		os.O_WRONLY|
			os.O_CREATE|
			os.O_TRUNC,
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

	if err := os.Chmod(
		destination,
		mode,
	); err != nil {
		return err
	}

	success = true

	return nil
}

func removeIfExists(
	path string,
) error {
	err := os.Remove(path)

	if err == nil ||
		os.IsNotExist(err) {
		return nil
	}

	return err
}

func syncDirectory(
	path string,
) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()

	return directory.Sync()
}

func logf(
	format string,
	args ...any,
) {
	message := fmt.Sprintf(
		format,
		args...,
	)

	fmt.Fprintf(
		os.Stderr,
		"[hiveworker-updater] %s %s\n",
		time.Now().
			UTC().
			Format(time.RFC3339),
		message,
	)
}

func fatal(
	format string,
	args ...any,
) {
	logf(
		"ERROR: "+format,
		args...,
	)

	os.Exit(1)
}
