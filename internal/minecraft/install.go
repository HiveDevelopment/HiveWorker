package minecraft

import (
	"bufio"
	"context"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"hash"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type File struct {
	Path   string            `json:"path"`
	URL    string            `json:"url"`
	Size   int64             `json:"size"`
	Hashes map[string]string `json:"hashes"`
}
type PreinstallRuntime struct {
	Parameters       map[string]string `json:"parameters,omitempty"`
	Steps            []RecipeStep      `json:"steps,omitempty"`
	InstallVariables map[string]string `json:"-"`
	Image            string            `json:"image"`
	Startup          string            `json:"startup"`
	Environment      map[string]string `json:"environment"`
	VariablesSchema  []map[string]any  `json:"variables_schema"`
	SecretKeys       []string          `json:"secretKeys"`
}
type Plan struct {
	Preset        string             `json:"preset,omitempty"`
	Version       string             `json:"version,omitempty"`
	Runtime       *PreinstallRuntime `json:"runtime,omitempty"`
	Format        int                `json:"format"`
	ID            string             `json:"id"`
	Name          string             `json:"name"`
	Kind          string             `json:"kind"`
	Minecraft     string             `json:"minecraft"`
	Loader        string             `json:"loader"`
	LoaderVersion string             `json:"loaderVersion"`
	Wipe          bool               `json:"wipe"`
	ReplaceMods   bool               `json:"replaceMods"`
	Files         []File             `json:"files"`
}
type Result struct {
	Image   string `json:"image,omitempty"`
	Startup string `json:"startup"`
	Backup  string `json:"backup"`
	Files   int    `json:"files"`
}

func SafePath(raw string) (string, error) {
	if raw == "" || len(raw) > 512 || strings.ContainsAny(raw, "\\:\x00\r\n") || filepath.IsAbs(raw) || (strings.EqualFold(raw, ".hivepanel") || strings.HasPrefix(strings.ToLower(raw), ".hivepanel/")) {
		return "", errors.New("unsafe content path")
	}
	for _, part := range strings.Split(raw, "/") {
		if part == ".." || part == "." || part == "" {
			return "", errors.New("unsafe content path")
		}
	}
	return filepath.FromSlash(raw), nil
}
func Validate(plan Plan, registry string) error {
	origin, err := url.Parse(registry)
	if err != nil || origin.Scheme != "https" || origin.Host == "" || origin.User != nil {
		return errors.New("Registry must use HTTPS")
	}
	if plan.Format != 1 || len(plan.Files) == 0 || len(plan.Files) > 600 {
		return errors.New("invalid content plan")
	}
	if plan.Kind != "mod" && plan.Kind != "plugin" && plan.Kind != "jar" && plan.Kind != "builder" && plan.Kind != "modpack" && plan.Kind != "preinstall" {
		return errors.New("invalid install type")
	}
	if (plan.Kind == "jar" || plan.Kind == "builder" || plan.Kind == "modpack") && plan.Loader != "vanilla" && plan.Loader != "paper" && plan.Loader != "fabric" && plan.Loader != "forge" && plan.Loader != "neoforge" {
		return errors.New("unsupported runtime")
	}
	if plan.Wipe && plan.Kind != "builder" && plan.Kind != "modpack" && plan.Kind != "preinstall" {
		return errors.New("full wipe is only allowed for packs and preinstalls")
	}
	if plan.Kind == "preinstall" {
		if !plan.Wipe || plan.Runtime == nil {
			return errors.New("preinstall requires a full replacement and runtime")
		}
		if err := validatePreinstallRuntime(plan.Runtime); err != nil {
			return err
		}
	} else if plan.Runtime != nil {
		return errors.New("runtime recipe is only valid for preinstalls")
	}
	seen := map[string]bool{}
	var size int64
	for _, file := range plan.Files {
		clean, err := SafePath(file.Path)
		if err != nil || seen[strings.ToLower(clean)] {
			return errors.New("unsafe or duplicate destination")
		}
		seen[strings.ToLower(clean)] = true
		if file.Size < 0 || file.Size > 512*1024*1024 {
			return errors.New("file too large")
		}
		size += file.Size
		remote, err := url.Parse(file.URL)
		if err != nil || remote.Scheme != origin.Scheme || remote.Host != origin.Host || remote.User != nil || (!strings.HasPrefix(remote.Path, "/api/v1/minecraft/artifacts/") && !strings.HasPrefix(remote.Path, "/api/v1/preinstalls/artifacts/")) || remote.RawQuery != "" || remote.Fragment != "" {
			return errors.New("download is not a Registry artifact")
		}
		suffix := remote.Path[strings.LastIndex(remote.Path, "/")+1:]
		if len(suffix) != 64 {
			return errors.New("invalid artifact ID")
		}
		if _, err := hex.DecodeString(suffix); err != nil {
			return err
		}
		if plan.Kind == "mod" && !strings.HasPrefix(file.Path, "mods/") || plan.Kind == "plugin" && !strings.HasPrefix(file.Path, "plugins/") {
			return errors.New("invalid install destination")
		}
		valid := false
		for algo, expected := range file.Hashes {
			length := map[string]int{"sha1": 40, "sha256": 64, "sha512": 128}[algo]
			if length > 0 && len(expected) == length {
				if _, err := hex.DecodeString(expected); err == nil {
					valid = true
				}
			}
		}
		if !valid {
			return errors.New("artifact checksum required")
		}
	}
	if size > 4*1024*1024*1024 {
		return errors.New("install too large")
	}
	return nil
}
func noSymlinks(root, relative string) error {
	path := root
	if info, err := os.Lstat(root); err != nil || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid Cell directory")
	}
	for _, part := range strings.Split(relative, string(filepath.Separator)) {
		path = filepath.Join(path, part)
		info, err := os.Lstat(path)
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return errors.New("destination contains a symlink")
		}
	}
	return nil
}
func download(ctx context.Context, file File, target string) error {
	client := &http.Client{Timeout: 10 * time.Minute, CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
		return errors.New("Registry artifact redirects are not allowed")
	}}
	request, err := http.NewRequestWithContext(ctx, "GET", file.URL, nil)
	if err != nil {
		return err
	}
	response, err := client.Do(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	if response.StatusCode != 200 {
		return fmt.Errorf("Registry returned %d", response.StatusCode)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
		return err
	}
	output, err := os.OpenFile(target, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0640)
	if err != nil {
		return err
	}
	defer output.Close()
	hashes := map[string]hash.Hash{"sha1": sha1.New(), "sha256": sha256.New(), "sha512": sha512.New()}
	writer := io.MultiWriter(output, hashes["sha1"], hashes["sha256"], hashes["sha512"])
	count, err := io.Copy(writer, io.LimitReader(response.Body, 512*1024*1024+1))
	if err != nil {
		return err
	}
	if count > 512*1024*1024 || file.Size > 0 && count != file.Size {
		return errors.New("artifact size mismatch")
	}
	for algo, expected := range file.Hashes {
		if digest, ok := hashes[algo]; ok && hex.EncodeToString(digest.Sum(nil)) != strings.ToLower(expected) {
			return errors.New("artifact checksum mismatch")
		}
	}
	return output.Sync()
}

type journalEntry struct {
	Path     string `json:"path"`
	Original bool   `json:"original"`
}
type journal struct {
	RolledBack bool           `json:"rolled_back,omitempty"`
	Entries    []journalEntry `json:"entries"`
	Committed  bool           `json:"committed"`
}

func saveJournal(backup string, state journal) error {
	data, _ := json.Marshal(state)
	temp := filepath.Join(backup, "journal.tmp")
	if err := os.WriteFile(temp, data, 0600); err != nil {
		return err
	}
	return os.Rename(temp, filepath.Join(backup, "journal.json"))
}
func rollback(root, backup string, state journal) error {
	for i := len(state.Entries) - 1; i >= 0; i-- {
		entry := state.Entries[i]
		path, err := SafePath(entry.Path)
		if err != nil {
			return err
		}
		if err := noSymlinks(root, path); err != nil {
			return err
		}
		target := filepath.Join(root, path)
		original := filepath.Join(backup, "original", path)
		if _, err := os.Lstat(original); err == nil {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
			if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
				return err
			}
			if err := os.Rename(original, target); err != nil {
				return err
			}
		} else if !entry.Original {
			if err := os.RemoveAll(target); err != nil {
				return err
			}
		}
	}
	return nil
}
func Recover(root string) error {
	if err := recoverRecipeContainers(root); err != nil {
		return err
	}
	directories, err := os.ReadDir(filepath.Join(root, ".hivepanel", "content-backups"))
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, directory := range directories {
		if !directory.IsDir() {
			continue
		}
		backup := filepath.Join(root, ".hivepanel", "content-backups", directory.Name())
		data, err := os.ReadFile(filepath.Join(backup, "journal.json"))
		if os.IsNotExist(err) {
			continue
		}
		if err != nil {
			return err
		}
		var state journal
		if err := json.Unmarshal(data, &state); err != nil {
			return err
		}
		if !state.Committed {
			if err := rollback(root, backup, state); err != nil {
				return err
			}
			state.Committed = true
			state.RolledBack = true
			if err := saveJournal(backup, state); err != nil {
				return err
			}
		}
	}
	return nil
}
func Install(ctx context.Context, root, image, registry string, plan Plan, log func(string), prepare func(string) error, uid, gid int, checkpoint func(Result) error) (Result, error) {
	if err := Validate(plan, registry); err != nil {
		return Result{}, err
	}
	if err := Recover(root); err != nil {
		return Result{}, err
	}
	if err := noSymlinks(root, filepath.Join(".hivepanel", "content-staging")); err != nil {
		return Result{}, err
	}
	stagingRoot := filepath.Join(root, ".hivepanel", "content-staging")
	if err := os.MkdirAll(stagingRoot, 0700); err != nil {
		return Result{}, err
	}
	stage, err := os.MkdirTemp(stagingRoot, "install-")
	if err != nil {
		return Result{}, err
	}
	defer os.RemoveAll(stage)
	for index, file := range plan.Files {
		select {
		case <-ctx.Done():
			return Result{}, ctx.Err()
		default:
		}
		clean, _ := SafePath(file.Path)
		log(fmt.Sprintf("Downloading %d/%d: %s", index+1, len(plan.Files), file.Path))
		if err := download(ctx, file, filepath.Join(stage, clean)); err != nil {
			return Result{}, fmt.Errorf("%s: %w", file.Path, err)
		}
	}
	installedImage := ""
	startup := ""
	if plan.Kind == "preinstall" {
		pinned, err := preparePreinstall(ctx, stage, plan.Runtime, log)
		if err != nil {
			return Result{}, err
		}
		installedImage = pinned
		if err := prepare(stage); err != nil {
			return Result{}, err
		}
		if err := runRecipe(ctx, stage, pinned, plan.Runtime.Steps, plan.Runtime.InstallVariables, uid, gid, log); err != nil {
			return Result{}, fmt.Errorf("recipe failed: %w", err)
		}
		startup = plan.Runtime.Startup
	}
	if plan.Kind == "jar" || plan.Kind == "builder" || plan.Kind == "modpack" {
		startup = "java -Xms128M -Xmx{{memory}}M -jar server.jar nogui"
		if plan.Loader == "forge" || plan.Loader == "neoforge" {
			if image == "" {
				return Result{}, errors.New("Cell has no Java image")
			}
			if err := prepare(stage); err != nil {
				return Result{}, err
			}
			log("Installing the mod loader inside the Cell’s Java image…")
			command := exec.CommandContext(ctx, "docker", "run", "--rm", "--user", fmt.Sprintf("%d:%d", uid, gid), "-v", stage+":/home/container", "-w", "/home/container", "--entrypoint", "java", image, "-jar", "installer.jar", "--installServer")
			pipe, err := command.StdoutPipe()
			if err != nil {
				return Result{}, err
			}
			command.Stderr = command.Stdout
			if err := command.Start(); err != nil {
				return Result{}, err
			}
			scanner := bufio.NewScanner(pipe)
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
				return Result{}, scanErr
			}
			if waitErr != nil {
				return Result{}, fmt.Errorf("loader installer failed: %w", waitErr)
			}
			if _, err := os.Stat(filepath.Join(stage, "run.sh")); err == nil {
				matches, _ := filepath.Glob(filepath.Join(stage, "libraries", "net", "minecraftforge", "forge", "*", "unix_args.txt"))
				if plan.Loader == "neoforge" {
					matches, _ = filepath.Glob(filepath.Join(stage, "libraries", "net", "neoforged", "neoforge", "*", "unix_args.txt"))
				}
				if len(matches) != 1 {
					return Result{}, errors.New("mod loader produced no unambiguous Java arguments file")
				}
				relative, _ := filepath.Rel(stage, matches[0])
				if _, err := SafePath(filepath.ToSlash(relative)); err != nil {
					return Result{}, err
				}
				startup = "java -Xms128M -Xmx{{memory}}M @" + filepath.ToSlash(relative) + " nogui"
			} else {
				matches, _ := filepath.Glob(filepath.Join(stage, "forge-*.jar"))
				found := false
				for _, candidate := range matches {
					if !strings.Contains(filepath.Base(candidate), "installer") {
						if err := os.Rename(candidate, filepath.Join(stage, "server.jar")); err != nil {
							return Result{}, err
						}
						found = true
						break
					}
				}
				if !found {
					return Result{}, errors.New("mod loader produced no server launcher")
				}
			}
		}
	}
	if err := prepare(stage); err != nil {
		return Result{}, err
	}
	var stagedSize int64
	var stagedFiles int
	// Top-level swaps must not hide links or unsafe generated paths inside directories.
	if err := filepath.WalkDir(stage, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == stage {
			return nil
		}
		if entry.Type()&os.ModeSymlink != 0 {
			if plan.Kind == "preinstall" {
				return safeStagedLink(stage, path)
			}
			return errors.New("installer produced symlink")
		}
		if entry.Type().IsRegular() {
			info, err := entry.Info()
			if err != nil {
				return err
			}
			stagedSize += info.Size()
			stagedFiles++
			if stagedSize > 4*1024*1024*1024 || stagedFiles > 100000 {
				return errors.New("staged installation exceeds file or size limits")
			}
		}
		relative, _ := filepath.Rel(stage, path)
		_, err = SafePath(filepath.ToSlash(relative))
		return err
	}); err != nil {
		return Result{}, err
	}
	backup := filepath.Join(root, ".hivepanel", "content-backups", fmt.Sprintf("%d", time.Now().UnixNano()))
	if err := noSymlinks(root, filepath.Join(".hivepanel", "content-backups")); err != nil {
		return Result{}, err
	}
	if err := os.MkdirAll(backup, 0700); err != nil {
		return Result{}, err
	}
	state := journal{}
	apply := func(relative string, removeOnly bool) error {
		clean, err := SafePath(relative)
		if err != nil {
			return err
		}
		if err := noSymlinks(root, clean); err != nil {
			return err
		}
		target, source, original := filepath.Join(root, clean), filepath.Join(stage, clean), filepath.Join(backup, "original", clean)
		_, statErr := os.Lstat(target)
		if statErr != nil && !os.IsNotExist(statErr) {
			return statErr
		}
		state.Entries = append(state.Entries, journalEntry{Path: relative, Original: statErr == nil})
		if err := saveJournal(backup, state); err != nil {
			return err
		}
		if statErr == nil {
			if err := os.MkdirAll(filepath.Dir(original), 0700); err != nil {
				return err
			}
			if err := os.Rename(target, original); err != nil {
				return err
			}
		}
		if err := os.MkdirAll(filepath.Dir(target), 0750); err != nil {
			return err
		}
		if removeOnly {
			return nil
		}
		return os.Rename(source, target)
	}
	failed := func(err error) (Result, error) {
		if rollbackErr := rollback(root, backup, state); rollbackErr != nil {
			return Result{}, fmt.Errorf("install failed (%v); restore backup %s: %w", err, backup, rollbackErr)
		}
		state.Committed = true
		state.RolledBack = true
		_ = saveJournal(backup, state)
		return Result{}, err
	}
	if !plan.Wipe && plan.ReplaceMods {
		if err := os.MkdirAll(filepath.Join(stage, "mods"), 0750); err != nil {
			return Result{}, err
		}
		if err := apply("mods", false); err != nil {
			return failed(err)
		}
	}
	if plan.Wipe {
		log("Replacing Cell files with the staged installation; previous files are retained in the rollback backup.")
		staged, readErr := os.ReadDir(stage)
		if readErr != nil {
			return failed(readErr)
		}
		existing, readErr := os.ReadDir(root)
		if readErr != nil {
			return failed(readErr)
		}
		sourceNames := map[string]bool{}
		allNames := map[string]bool{}
		for _, entry := range staged {
			sourceNames[entry.Name()] = true
			allNames[entry.Name()] = true
		}
		for _, entry := range existing {
			if strings.EqualFold(entry.Name(), ".hivepanel") {
				continue
			}
			allNames[entry.Name()] = true
		}
		names := make([]string, 0, len(allNames))
		for name := range allNames {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			if err = apply(name, !sourceNames[name]); err != nil {
				break
			}
		}
	} else {
		err = filepath.WalkDir(stage, func(path string, item os.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if path == stage {
				return nil
			}
			if item.Type()&os.ModeSymlink != 0 {
				if plan.Kind == "preinstall" {
					return safeStagedLink(stage, path)
				}
				return errors.New("installer produced symlink")
			}
			if item.IsDir() {
				return nil
			}
			relative, _ := filepath.Rel(stage, path)
			return apply(filepath.ToSlash(relative), false)
		})
	}

	if err != nil {
		return failed(err)
	}
	result := Result{Startup: startup, Image: installedImage, Backup: backup, Files: stagedFiles}
	if checkpoint != nil {
		if err := checkpoint(result); err != nil {
			return failed(err)
		}
	}
	state.Committed = true
	if err := saveJournal(backup, state); err != nil {
		return failed(err)
	}
	log("Installation completed. Previous files are retained in the content backup.")
	return Result{Startup: startup, Image: installedImage, Backup: backup, Files: stagedFiles}, nil
}

// TransactionCommitted is checked before recovery marks rolled-back journals as resolved.
func TransactionCommitted(backup string) bool {
	data, err := os.ReadFile(filepath.Join(backup, "journal.json"))
	if err != nil {
		return false
	}
	var state journal
	return json.Unmarshal(data, &state) == nil && state.Committed && !state.RolledBack
}
