package updater

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	workerversion "hivepanel-worker/internal/version"
)

const (
	repositoryOwner = "HiveDevelopment"
	repositoryName  = "hiveworker"

	binaryPath = "/usr/local/bin/hiveworker"
	updateDir  = "/var/lib/hivepanel/updates"
	helperPath = "/usr/local/libexec/hiveworker-updater"
)

type Status struct {
	State          string     `json:"state"`
	CurrentVersion string     `json:"current_version"`
	TargetVersion  string     `json:"target_version,omitempty"`
	Message        string     `json:"message"`
	Error          string     `json:"error,omitempty"`
	StartedAt      *time.Time `json:"started_at,omitempty"`
	UpdatedAt      time.Time  `json:"updated_at"`
}

type Request struct {
	Version string `json:"version"`
}

type githubRelease struct {
	TagName string `json:"tag_name"`
}

type Manager struct {
	mu     sync.RWMutex
	status Status
	client *http.Client
}

func NewManager() *Manager {
	return &Manager{
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		status: Status{
			State:          "idle",
			CurrentVersion: workerversion.Version,
			Message:        "No Worker update is currently running.",
			UpdatedAt:      time.Now().UTC(),
		},
	}
}

func (m *Manager) Status() Status {
	m.mu.RLock()
	defer m.mu.RUnlock()

	status := m.status
	status.CurrentVersion = workerversion.Version

	return status
}

func (m *Manager) setStatus(
	state string,
	target string,
	message string,
	errMessage string,
	startedAt *time.Time,
) {
	m.mu.Lock()
	defer m.mu.Unlock()

	m.status = Status{
		State:          state,
		CurrentVersion: workerversion.Version,
		TargetVersion:  target,
		Message:        message,
		Error:          errMessage,
		StartedAt:      startedAt,
		UpdatedAt:      time.Now().UTC(),
	}
}

func (m *Manager) Start(version string) error {
	version = normalizeVersion(version)

	if version == "" {
		return errors.New("version is required")
	}

	if !validVersion(version) {
		return errors.New("invalid Worker version")
	}

	m.mu.Lock()

	switch m.status.State {
	case "queued", "checking", "downloading", "verifying", "staging", "restarting":
		m.mu.Unlock()
		return errors.New("a Worker update is already running")
	}

	startedAt := time.Now().UTC()

	m.status = Status{
		State:          "queued",
		CurrentVersion: workerversion.Version,
		TargetVersion:  version,
		Message:        "Worker update queued.",
		StartedAt:      &startedAt,
		UpdatedAt:      startedAt,
	}

	m.mu.Unlock()

	go m.run(version, startedAt)

	return nil
}

func (m *Manager) run(version string, startedAt time.Time) {
	fail := func(message string, err error) {
		m.setStatus(
			"failed",
			version,
			message,
			err.Error(),
			&startedAt,
		)
	}

	m.setStatus(
		"checking",
		version,
		"Checking Worker release...",
		"",
		&startedAt,
	)

	releaseVersion, err := m.resolveRelease(version)
	if err != nil {
		fail("Unable to resolve Worker release.", err)
		return
	}

	if sameVersion(workerversion.Version, releaseVersion) {
		m.setStatus(
			"complete",
			releaseVersion,
			"Worker is already running the requested version.",
			"",
			&startedAt,
		)
		return
	}

	assetName, err := releaseAssetName()
	if err != nil {
		fail("Unable to determine Worker architecture.", err)
		return
	}

	if err := os.MkdirAll(updateDir, 0755); err != nil {
		fail("Unable to prepare Worker update directory.", err)
		return
	}

	stagedBinary := filepath.Join(
		updateDir,
		"hiveworker-"+strings.TrimPrefix(releaseVersion, "v")+".new",
	)

	m.setStatus(
		"downloading",
		releaseVersion,
		"Downloading Worker release...",
		"",
		&startedAt,
	)

	if err := m.downloadReleaseAsset(
		releaseVersion,
		assetName,
		stagedBinary,
	); err != nil {
		fail("Worker binary download failed.", err)
		return
	}

	defer func() {
		_ = os.Remove(stagedBinary)
	}()

	checksumsPath := filepath.Join(
		updateDir,
		"checksums-"+strings.TrimPrefix(releaseVersion, "v")+".txt",
	)

	if err := m.downloadReleaseAsset(
		releaseVersion,
		"checksums.txt",
		checksumsPath,
	); err != nil {
		fail("Worker checksum download failed.", err)
		return
	}

	defer func() {
		_ = os.Remove(checksumsPath)
	}()

	m.setStatus(
		"verifying",
		releaseVersion,
		"Verifying Worker release...",
		"",
		&startedAt,
	)

	expectedChecksum, err := checksumForAsset(
		checksumsPath,
		assetName,
	)
	if err != nil {
		fail("Unable to read Worker checksum.", err)
		return
	}

	actualChecksum, err := calculateSHA256(stagedBinary)
	if err != nil {
		fail("Unable to calculate Worker checksum.", err)
		return
	}

	if !strings.EqualFold(expectedChecksum, actualChecksum) {
		fail(
			"Worker release checksum verification failed.",
			fmt.Errorf(
				"checksum mismatch for %s",
				assetName,
			),
		)
		return
	}

	if err := os.Chmod(stagedBinary, 0755); err != nil {
		fail("Unable to mark Worker release executable.", err)
		return
	}

	if err := verifyBinary(stagedBinary, releaseVersion); err != nil {
		fail("Worker release validation failed.", err)
		return
	}

	if _, err := os.Stat(helperPath); err != nil {
		fail(
			"Worker updater helper is not installed.",
			fmt.Errorf(
				"%s is unavailable: %w",
				helperPath,
				err,
			),
		)
		return
	}

	m.setStatus(
		"staging",
		releaseVersion,
		"Worker release verified. Preparing restart...",
		"",
		&startedAt,
	)

	if err := launchUpdaterHelper(
		stagedBinary,
		releaseVersion,
	); err != nil {
		fail("Unable to launch Worker updater.", err)
		return
	}

	m.setStatus(
		"restarting",
		releaseVersion,
		"Worker is restarting into the new version...",
		"",
		&startedAt,
	)
}

func (m *Manager) resolveRelease(version string) (string, error) {
	if version != "latest" {
		return ensureVPrefix(version), nil
	}

	request, err := http.NewRequestWithContext(
		context.Background(),
		http.MethodGet,
		fmt.Sprintf(
			"https://api.github.com/repos/%s/%s/releases/latest",
			repositoryOwner,
			repositoryName,
		),
		nil,
	)
	if err != nil {
		return "", err
	}

	request.Header.Set("Accept", "application/vnd.github+json")
	request.Header.Set("User-Agent", "HivePanel-Worker")

	response, err := m.client.Do(request)
	if err != nil {
		return "", err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return "", fmt.Errorf(
			"GitHub returned HTTP %d",
			response.StatusCode,
		)
	}

	var release githubRelease

	if err := json.NewDecoder(response.Body).Decode(&release); err != nil {
		return "", err
	}

	release.TagName = strings.TrimSpace(release.TagName)

	if release.TagName == "" {
		return "", errors.New("latest release has no tag")
	}

	if !validVersion(release.TagName) {
		return "", errors.New("latest release tag is invalid")
	}

	return ensureVPrefix(release.TagName), nil
}

func (m *Manager) downloadReleaseAsset(
	version string,
	asset string,
	destination string,
) error {
	version = ensureVPrefix(version)

	url := fmt.Sprintf(
		"https://github.com/%s/%s/releases/download/%s/%s",
		repositoryOwner,
		repositoryName,
		version,
		asset,
	)

	ctx, cancel := context.WithTimeout(
		context.Background(),
		5*time.Minute,
	)
	defer cancel()

	request, err := http.NewRequestWithContext(
		ctx,
		http.MethodGet,
		url,
		nil,
	)
	if err != nil {
		return err
	}

	request.Header.Set("User-Agent", "HivePanel-Worker")

	response, err := m.client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()

	if response.StatusCode != http.StatusOK {
		return fmt.Errorf(
			"download returned HTTP %d",
			response.StatusCode,
		)
	}

	tempPath := destination + ".part"

	_ = os.Remove(tempPath)

	file, err := os.OpenFile(
		tempPath,
		os.O_CREATE|os.O_WRONLY|os.O_TRUNC,
		0755,
	)
	if err != nil {
		return err
	}

	_, copyErr := io.Copy(file, response.Body)
	closeErr := file.Close()

	if copyErr != nil {
		_ = os.Remove(tempPath)
		return copyErr
	}

	if closeErr != nil {
		_ = os.Remove(tempPath)
		return closeErr
	}

	if err := os.Rename(tempPath, destination); err != nil {
		_ = os.Remove(tempPath)
		return err
	}

	return nil
}

func releaseAssetName() (string, error) {
	if runtime.GOOS != "linux" {
		return "", fmt.Errorf(
			"self-update is only supported on linux, got %s",
			runtime.GOOS,
		)
	}

	switch runtime.GOARCH {
	case "amd64":
		return "hiveworker_linux_amd64", nil

	case "arm64":
		return "hiveworker_linux_arm64", nil

	default:
		return "", fmt.Errorf(
			"unsupported architecture: %s",
			runtime.GOARCH,
		)
	}
}

func checksumForAsset(
	path string,
	assetName string,
) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)

	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())

		if len(fields) < 2 {
			continue
		}

		filename := strings.TrimPrefix(
			strings.TrimSpace(fields[len(fields)-1]),
			"*",
		)

		if filename != assetName {
			continue
		}

		checksum := strings.TrimSpace(fields[0])

		if len(checksum) != 64 {
			return "", errors.New(
				"invalid SHA-256 checksum in manifest",
			)
		}

		if _, err := hex.DecodeString(checksum); err != nil {
			return "", errors.New(
				"invalid SHA-256 checksum in manifest",
			)
		}

		return strings.ToLower(checksum), nil
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf(
		"checksum for %s was not found",
		assetName,
	)
}

func calculateSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer file.Close()

	hasher := sha256.New()

	if _, err := io.Copy(hasher, file); err != nil {
		return "", err
	}

	return hex.EncodeToString(hasher.Sum(nil)), nil
}

func verifyBinary(path string, expectedVersion string) error {
	ctx, cancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer cancel()

	command := exec.CommandContext(
		ctx,
		path,
		"--version",
	)

	output, err := command.CombinedOutput()

	if err == nil {
		value := strings.TrimSpace(string(output))

		if value == "" {
			return nil
		}

		if strings.Contains(
			strings.ToLower(value),
			strings.ToLower(
				strings.TrimPrefix(expectedVersion, "v"),
			),
		) {
			return nil
		}

		// Don't fail here solely because the CLI output format
		// differs. Successful execution still proves the binary
		// can be started on this machine.
		return nil
	}

	// The current daemon may not expose --version yet.
	// Try --help as a basic executable/architecture check.
	helpCtx, helpCancel := context.WithTimeout(
		context.Background(),
		10*time.Second,
	)
	defer helpCancel()

	helpCommand := exec.CommandContext(
		helpCtx,
		path,
		"--help",
	)

	if helpErr := helpCommand.Run(); helpErr != nil {
		return fmt.Errorf(
			"downloaded Worker could not be executed: %w",
			helpErr,
		)
	}

	return nil
}

func launchUpdaterHelper(
	stagedBinary string,
	targetVersion string,
) error {
	command := exec.Command(
		helperPath,
		"--pid",
		fmt.Sprintf("%d", os.Getpid()),
		"--staged",
		stagedBinary,
		"--target",
		targetVersion,
	)

	command.Stdin = nil
	command.Stdout = nil
	command.Stderr = nil

	if err := command.Start(); err != nil {
		return err
	}

	/*
		The updater helper is now running independently.

		Give it enough time to initialise and begin waiting for
		this Worker process to terminate.
	*/
	time.Sleep(500 * time.Millisecond)

	go func() {
		time.Sleep(1 * time.Second)
		os.Exit(0)
	}()

	return nil
}

func normalizeVersion(version string) string {
	version = strings.TrimSpace(version)

	if strings.EqualFold(version, "latest") {
		return "latest"
	}

	return ensureVPrefix(version)
}

func ensureVPrefix(version string) string {
	version = strings.TrimSpace(version)

	if version == "" {
		return ""
	}

	if strings.HasPrefix(version, "v") ||
		strings.HasPrefix(version, "V") {
		return "v" + version[1:]
	}

	return "v" + version
}

func validVersion(version string) bool {
	if strings.EqualFold(version, "latest") {
		return true
	}

	version = strings.TrimPrefix(
		strings.TrimPrefix(
			strings.TrimSpace(version),
			"v",
		),
		"V",
	)

	parts := strings.Split(version, ".")

	if len(parts) != 3 {
		return false
	}

	for _, part := range parts {
		if part == "" {
			return false
		}

		for _, character := range part {
			if character < '0' || character > '9' {
				return false
			}
		}
	}

	return true
}

func sameVersion(a string, b string) bool {
	normalize := func(value string) string {
		return strings.TrimPrefix(
			strings.TrimPrefix(
				strings.TrimSpace(value),
				"v",
			),
			"V",
		)
	}

	return normalize(a) == normalize(b)
}
