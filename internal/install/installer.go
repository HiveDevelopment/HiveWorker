package install

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"

	"hivepanel-worker/internal/comb"
	"hivepanel-worker/internal/files"
)

type Logger func(line string)

type Context struct {
	InstanceDir string
	Variables   map[string]string
	Saved       map[string]any
	Log         Logger
}

func Run(
	instanceDir string,
	variables map[string]string,
	steps []comb.InstallStep,
	log Logger,
) error {
	ctx := &Context{
		InstanceDir: instanceDir,
		Variables:   variables,
		Saved:       map[string]any{},
		Log:         log,
	}

	for index, step := range steps {
		log(fmt.Sprintf(
			"Running install step %d/%d: %s",
			index+1,
			len(steps),
			step.Type,
		))

		result, err := runStep(ctx, step)
		if err != nil {
			return fmt.Errorf(
				"install step %d (%s) failed: %w",
				index+1,
				step.Type,
				err,
			)
		}

		if step.Save != "" {
			ctx.Saved[step.Save] = result
		}
	}

	return nil
}

func runStep(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	switch step.Type {
	case "mkdir":
		return stepMkdir(ctx, step)

	case "write_file":
		return stepWriteFile(ctx, step)

	case "download":
		return stepDownload(ctx, step)

	case "run":
		return stepRun(ctx, step)

	case "http":
		return stepHTTP(ctx, step)

	case "chmod":
		return stepChmod(ctx, step)

	case "move":
		return stepMove(ctx, step)

	case "copy":
		return stepCopy(ctx, step)

	case "delete":
		return stepDelete(ctx, step)

	case "extract":
		return stepExtract(ctx, step)

	case "git":
		return stepGit(ctx, step)

	case "steamcmd":
		return stepSteamCMD(ctx, step)

	default:
		return nil, errors.New(
			"unknown install step type: " + step.Type,
		)
	}
}

func stepMkdir(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	target := render(
		ctx,
		getString(step.With, "target"),
	)

	if target == "" {
		return nil, errors.New(
			"mkdir target is required",
		)
	}

	ctx.Log("Creating folder: " + target)

	path, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	return map[string]any{
		"target": target,
	}, os.MkdirAll(path, 0755)
}

func stepWriteFile(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	target := render(
		ctx,
		getString(step.With, "target"),
	)

	content := render(
		ctx,
		getString(step.With, "content"),
	)

	if target == "" {
		return nil, errors.New(
			"write_file target is required",
		)
	}

	ctx.Log("Writing file: " + target)

	err := files.Write(
		ctx.InstanceDir,
		target,
		content,
	)

	return map[string]any{
		"target": target,
		"bytes":  len(content),
	}, err
}

func stepDownload(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	url := render(
		ctx,
		getString(step.With, "url"),
	)

	target := render(
		ctx,
		getString(step.With, "target"),
	)

	if url == "" {
		return nil, errors.New(
			"download url is required",
		)
	}

	if target == "" {
		return nil, errors.New(
			"download target is required",
		)
	}

	ctx.Log("Downloading: " + url)

	response, err := httpGet(url)
	if err != nil {
		return nil, err
	}

	defer response.Body.Close()

	if response.StatusCode < 200 ||
		response.StatusCode >= 300 {
		return nil, errors.New(
			"download failed with status: " +
				response.Status,
		)
	}

	targetPath, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(
		filepath.Dir(targetPath),
		0755,
	); err != nil {
		return nil, err
	}

	out, err := os.Create(targetPath)
	if err != nil {
		return nil, err
	}

	defer out.Close()

	size, err := io.Copy(
		out,
		response.Body,
	)
	if err != nil {
		return nil, err
	}

	ctx.Log("Downloaded to: " + target)

	return map[string]any{
		"url":    url,
		"target": target,
		"bytes":  size,
	}, nil
}

func stepRun(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	command := render(
		ctx,
		getString(step.With, "command"),
	)

	if command == "" {
		return nil, errors.New(
			"run command is required",
		)
	}

	ctx.Log("Running command: " + command)

	cmd := shellCommand(command)
	cmd.Dir = ctx.InstanceDir

	output, err := cmd.CombinedOutput()

	if len(output) > 0 {
		ctx.Log(
			strings.TrimSpace(
				string(output),
			),
		)
	}

	return map[string]any{
		"command": command,
		"output":  string(output),
	}, err
}

func stepHTTP(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	url := render(
		ctx,
		getString(step.With, "url"),
	)

	if url == "" {
		return nil, errors.New(
			"http url is required",
		)
	}

	ctx.Log("HTTP GET: " + url)

	response, err := httpGet(url)
	if err != nil {
		return nil, err
	}

	defer response.Body.Close()

	if response.StatusCode < 200 ||
		response.StatusCode >= 300 {
		return nil, errors.New(
			"http request failed with status: " +
				response.Status,
		)
	}

	var data any

	if err := json.NewDecoder(
		response.Body,
	).Decode(&data); err != nil {
		return nil, err
	}

	return data, nil
}

func stepChmod(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	target := render(
		ctx,
		getString(step.With, "target"),
	)

	modeValue := render(
		ctx,
		getString(step.With, "mode"),
	)

	if target == "" || modeValue == "" {
		return nil, errors.New(
			"chmod target and mode are required",
		)
	}

	mode, err := strconv.ParseUint(
		strings.TrimPrefix(
			modeValue,
			"0",
		),
		8,
		32,
	)
	if err != nil {
		return nil, fmt.Errorf(
			"invalid chmod mode %q: %w",
			modeValue,
			err,
		)
	}

	targetPath, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	ctx.Log(
		"Changing permissions: " + target,
	)

	return map[string]any{
			"target": target,
			"mode":   modeValue,
		}, os.Chmod(
			targetPath,
			os.FileMode(mode),
		)
}

func stepMove(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	source := render(
		ctx,
		getString(step.With, "source"),
	)

	target := render(
		ctx,
		getString(step.With, "target"),
	)

	if source == "" || target == "" {
		return nil, errors.New(
			"move source and target are required",
		)
	}

	sourcePath, err := files.SafePath(
		ctx.InstanceDir,
		source,
	)
	if err != nil {
		return nil, err
	}

	targetPath, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(
		filepath.Dir(targetPath),
		0755,
	); err != nil {
		return nil, err
	}

	ctx.Log(
		"Moving: " +
			source +
			" -> " +
			target,
	)

	return map[string]any{
			"source": source,
			"target": target,
		}, os.Rename(
			sourcePath,
			targetPath,
		)
}

func stepCopy(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	source := render(
		ctx,
		getString(step.With, "source"),
	)

	target := render(
		ctx,
		getString(step.With, "target"),
	)

	if source == "" || target == "" {
		return nil, errors.New(
			"copy source and target are required",
		)
	}

	sourcePath, err := files.SafePath(
		ctx.InstanceDir,
		source,
	)
	if err != nil {
		return nil, err
	}

	targetPath, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	ctx.Log(
		"Copying: " +
			source +
			" -> " +
			target,
	)

	if err := copyPath(
		sourcePath,
		targetPath,
	); err != nil {
		return nil, err
	}

	return map[string]any{
		"source": source,
		"target": target,
	}, nil
}

func stepDelete(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	target := render(
		ctx,
		getString(step.With, "target"),
	)

	if target == "" ||
		target == "." ||
		target == "/" {
		return nil, errors.New(
			"delete target is invalid",
		)
	}

	targetPath, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	ctx.Log("Deleting: " + target)

	return map[string]any{
		"target": target,
	}, os.RemoveAll(targetPath)
}

func stepExtract(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	source := render(
		ctx,
		getString(step.With, "source"),
	)

	target := render(
		ctx,
		getString(step.With, "target"),
	)

	if source == "" {
		return nil, errors.New(
			"extract source is required",
		)
	}

	if target == "" {
		target = "."
	}

	sourcePath, err := files.SafePath(
		ctx.InstanceDir,
		source,
	)
	if err != nil {
		return nil, err
	}

	targetPath, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(
		targetPath,
		0755,
	); err != nil {
		return nil, err
	}

	ctx.Log(
		"Extracting: " +
			source +
			" -> " +
			target,
	)

	lower := strings.ToLower(source)

	switch {
	case strings.HasSuffix(
		lower,
		".zip",
	):
		err = extractZip(
			sourcePath,
			targetPath,
		)

	case strings.HasSuffix(
		lower,
		".tar.gz",
	),
		strings.HasSuffix(
			lower,
			".tgz",
		):
		err = extractTarGz(
			sourcePath,
			targetPath,
		)

	default:
		err = errors.New(
			"unsupported archive format; supported formats are .zip, .tar.gz and .tgz",
		)
	}

	return map[string]any{
		"source": source,
		"target": target,
	}, err
}

func stepGit(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	repository := render(
		ctx,
		getString(
			step.With,
			"repository",
		),
	)

	target := render(
		ctx,
		getString(step.With, "target"),
	)

	branch := render(
		ctx,
		getString(step.With, "branch"),
	)

	if repository == "" {
		repository = render(
			ctx,
			getString(step.With, "url"),
		)
	}

	if repository == "" {
		return nil, errors.New(
			"git repository is required",
		)
	}

	if target == "" {
		target = "."
	}

	if _, err := exec.LookPath("git"); err != nil {
		return nil, errors.New(
			"git is not installed on the worker host",
		)
	}

	targetPath, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	args := []string{
		"clone",
		"--depth",
		"1",
	}

	if branch != "" {
		args = append(
			args,
			"--branch",
			branch,
		)
	}

	args = append(
		args,
		repository,
		targetPath,
	)

	ctx.Log(
		"Cloning Git repository: " +
			repository,
	)

	cmd := exec.Command(
		"git",
		args...,
	)

	output, err := cmd.CombinedOutput()

	if len(output) > 0 {
		ctx.Log(
			strings.TrimSpace(
				string(output),
			),
		)
	}

	return map[string]any{
		"repository": repository,
		"target":     target,
		"output":     string(output),
	}, err
}

func stepSteamCMD(
	ctx *Context,
	step comb.InstallStep,
) (any, error) {
	appID := render(
		ctx,
		getString(step.With, "app_id"),
	)

	target := render(
		ctx,
		getString(step.With, "directory"),
	)

	beta := render(
		ctx,
		getString(step.With, "beta"),
	)

	username := render(
		ctx,
		getString(step.With, "username"),
	)

	password := render(
		ctx,
		getString(step.With, "password"),
	)

	validate := getBool(
		step.With,
		"validate",
		true,
	)

	image := render(
		ctx,
		getString(step.With, "image"),
	)

	if appID == "" {
		return nil, errors.New(
			"steamcmd app_id is required",
		)
	}

	if target == "" ||
		target == "." ||
		target == "/home/container" {
		target = "."
	}

	/*
		HivePanel's own SteamCMD image is the default.

		Combs can still override this using the
		"image" property on the steamcmd install step.
	*/
	if image == "" {
		image =
			"ghcr.io/hivedevelopment/hivepanel-steamcmd:latest"
	}

	if _, err := exec.LookPath("docker"); err != nil {
		return nil, errors.New(
			"docker is required for steamcmd installation",
		)
	}

	targetPath, err := files.SafePath(
		ctx.InstanceDir,
		target,
	)
	if err != nil {
		return nil, err
	}

	if err := os.MkdirAll(
		targetPath,
		0755,
	); err != nil {
		return nil, err
	}

	login := []string{
		"+login",
		"anonymous",
	}

	if username != "" {
		login = []string{
			"+login",
			username,
		}

		if password != "" {
			login = append(
				login,
				password,
			)
		}
	}

	/*
		The host Cell directory is mounted at
		/home/container/server.

		SteamCMD itself lives separately at:
		/home/container/steamcmd/steamcmd.sh

		This prevents the Cell mount from hiding
		the SteamCMD installation inside the image.
	*/
	const containerInstallDir = "/home/container/server"

	const steamCMDPath = "/home/container/steamcmd/steamcmd.sh"

	steamArgs := []string{
		"+force_install_dir",
		containerInstallDir,
	}

	steamArgs = append(
		steamArgs,
		login...,
	)

	if beta != "" {
		steamArgs = append(
			steamArgs,
			"+app_update",
			appID,
			"-beta",
			beta,
		)
	} else {
		steamArgs = append(
			steamArgs,
			"+app_update",
			appID,
		)
	}

	if validate {
		steamArgs = append(
			steamArgs,
			"validate",
		)
	}

	steamArgs = append(
		steamArgs,
		"+quit",
	)

	args := []string{
		"run",
		"--rm",

		"-v",
		targetPath +
			":" +
			containerInstallDir,

		image,

		steamCMDPath,
	}

	args = append(
		args,
		steamArgs...,
	)

	ctx.Log(
		"Installing Steam application " +
			appID +
			" with SteamCMD",
	)

	cmd := exec.Command(
		"docker",
		args...,
	)

	output, err := cmd.CombinedOutput()

	if len(output) > 0 {
		ctx.Log(
			strings.TrimSpace(
				string(output),
			),
		)
	}

	if err != nil {
		message := strings.TrimSpace(
			string(output),
		)

		/*
			Include SteamCMD/Docker output in the
			error returned to HivePanel.

			This is considerably more useful than
			only returning "exit status 1/127".
		*/
		if message != "" {
			return nil, fmt.Errorf(
				"steamcmd failed: %w: %s",
				err,
				message,
			)
		}

		return nil, fmt.Errorf(
			"steamcmd failed: %w",
			err,
		)
	}

	return map[string]any{
		"app_id":    appID,
		"directory": target,
		"image":     image,
		"output":    string(output),
	}, nil
}

func copyPath(
	source string,
	target string,
) error {
	info, err := os.Lstat(source)
	if err != nil {
		return err
	}

	if info.IsDir() {
		if err := os.MkdirAll(
			target,
			info.Mode().Perm(),
		); err != nil {
			return err
		}

		entries, err := os.ReadDir(source)
		if err != nil {
			return err
		}

		for _, entry := range entries {
			if err := copyPath(
				filepath.Join(
					source,
					entry.Name(),
				),
				filepath.Join(
					target,
					entry.Name(),
				),
			); err != nil {
				return err
			}
		}

		return nil
	}

	if !info.Mode().IsRegular() {
		return errors.New(
			"copy only supports regular files and directories",
		)
	}

	if err := os.MkdirAll(
		filepath.Dir(target),
		0755,
	); err != nil {
		return err
	}

	in, err := os.Open(source)
	if err != nil {
		return err
	}

	defer in.Close()

	out, err := os.OpenFile(
		target,
		os.O_CREATE|
			os.O_TRUNC|
			os.O_WRONLY,
		info.Mode().Perm(),
	)
	if err != nil {
		return err
	}

	defer out.Close()

	_, err = io.Copy(
		out,
		in,
	)

	return err
}

func extractZip(
	source string,
	target string,
) error {
	reader, err := zip.OpenReader(source)
	if err != nil {
		return err
	}

	defer reader.Close()

	for _, entry := range reader.File {
		destination, err := safeArchivePath(
			target,
			entry.Name,
		)
		if err != nil {
			return err
		}

		if entry.FileInfo().IsDir() {
			if err := os.MkdirAll(
				destination,
				0755,
			); err != nil {
				return err
			}

			continue
		}

		if entry.Mode()&os.ModeSymlink != 0 {
			return errors.New(
				"archive symlinks are not supported",
			)
		}

		if err := os.MkdirAll(
			filepath.Dir(destination),
			0755,
		); err != nil {
			return err
		}

		sourceFile, err := entry.Open()
		if err != nil {
			return err
		}

		destinationFile, err := os.OpenFile(
			destination,
			os.O_CREATE|
				os.O_TRUNC|
				os.O_WRONLY,
			entry.Mode().Perm(),
		)
		if err != nil {
			sourceFile.Close()
			return err
		}

		_, copyErr := io.Copy(
			destinationFile,
			sourceFile,
		)

		closeErr :=
			destinationFile.Close()

		sourceFile.Close()

		if copyErr != nil {
			return copyErr
		}

		if closeErr != nil {
			return closeErr
		}
	}

	return nil
}

func extractTarGz(
	source string,
	target string,
) error {
	file, err := os.Open(source)
	if err != nil {
		return err
	}

	defer file.Close()

	gzipReader, err := gzip.NewReader(file)
	if err != nil {
		return err
	}

	defer gzipReader.Close()

	tarReader := tar.NewReader(
		gzipReader,
	)

	for {
		header, err := tarReader.Next()

		if errors.Is(err, io.EOF) {
			break
		}

		if err != nil {
			return err
		}

		destination, err := safeArchivePath(
			target,
			header.Name,
		)
		if err != nil {
			return err
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(
				destination,
				0755,
			); err != nil {
				return err
			}

		case tar.TypeReg, tar.TypeRegA:
			if err := os.MkdirAll(
				filepath.Dir(
					destination,
				),
				0755,
			); err != nil {
				return err
			}

			output, err := os.OpenFile(
				destination,
				os.O_CREATE|
					os.O_TRUNC|
					os.O_WRONLY,
				os.FileMode(
					header.Mode,
				).Perm(),
			)
			if err != nil {
				return err
			}

			_, copyErr := io.Copy(
				output,
				tarReader,
			)

			closeErr := output.Close()

			if copyErr != nil {
				return copyErr
			}

			if closeErr != nil {
				return closeErr
			}

		default:
			return fmt.Errorf(
				"unsupported archive entry type for %s",
				header.Name,
			)
		}
	}

	return nil
}

func safeArchivePath(
	root string,
	name string,
) (string, error) {
	cleaned := filepath.Clean(
		filepath.FromSlash(name),
	)

	if cleaned == "." ||
		filepath.IsAbs(cleaned) ||
		cleaned == ".." ||
		strings.HasPrefix(
			cleaned,
			".."+string(os.PathSeparator),
		) {
		return "", fmt.Errorf(
			"archive entry %q escapes the destination",
			name,
		)
	}

	destination := filepath.Join(
		root,
		cleaned,
	)

	relative, err := filepath.Rel(
		root,
		destination,
	)

	if err != nil ||
		relative == ".." ||
		strings.HasPrefix(
			relative,
			".."+string(os.PathSeparator),
		) {
		return "", fmt.Errorf(
			"archive entry %q escapes the destination",
			name,
		)
	}

	return destination, nil
}

func getString(
	values map[string]any,
	key string,
) string {
	if values == nil {
		return ""
	}

	value, exists := values[key]

	if !exists || value == nil {
		return ""
	}

	return fmt.Sprint(value)
}

func getBool(
	values map[string]any,
	key string,
	fallback bool,
) bool {
	if values == nil {
		return fallback
	}

	value, exists := values[key]

	if !exists || value == nil {
		return fallback
	}

	switch typed := value.(type) {
	case bool:
		return typed

	case string:
		parsed, err :=
			strconv.ParseBool(typed)

		if err == nil {
			return parsed
		}
	}

	return fallback
}

func render(
	ctx *Context,
	input string,
) string {
	output := input

	for key, value := range ctx.Variables {
		output = strings.ReplaceAll(
			output,
			"{{"+key+"}}",
			value,
		)
	}

	for key, value := range ctx.Saved {
		flattened := flatten(
			key,
			value,
		)

		for flatKey, flatValue := range flattened {
			output = strings.ReplaceAll(
				output,
				"{{"+flatKey+"}}",
				flatValue,
			)
		}
	}

	return output
}

func flatten(
	prefix string,
	value any,
) map[string]string {
	result := map[string]string{}

	switch typed := value.(type) {
	case map[string]any:
		for key, nested := range typed {
			for nestedKey, nestedValue := range flatten(
				prefix+"."+key,
				nested,
			) {
				result[nestedKey] =
					nestedValue
			}
		}

	case []any:
		for index, nested := range typed {
			for nestedKey, nestedValue := range flatten(
				fmt.Sprintf(
					"%s.%d",
					prefix,
					index,
				),
				nested,
			) {
				result[nestedKey] =
					nestedValue
			}
		}

	default:
		result[prefix] =
			fmt.Sprint(typed)
	}

	return result
}

func shellCommand(
	command string,
) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command(
			"powershell",
			"-Command",
			command,
		)
	}

	return exec.Command(
		"bash",
		"-lc",
		command,
	)
}

func httpGet(
	url string,
) (*http.Response, error) {
	request, err := http.NewRequest(
		"GET",
		url,
		nil,
	)
	if err != nil {
		return nil, err
	}

	request.Header.Set(
		"User-Agent",
		"HivePanel-Worker/1.0",
	)

	return http.DefaultClient.Do(request)
}
