package minecraft

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var serverPortLine = regexp.MustCompile(`(?m)^\s*server-port\s*[:=][^\r\n]*`)
var acceptedEULA = regexp.MustCompile(`(?m)^\s*eula\s*=\s*true\s*$`)

func EULAAccepted(root string) bool {
	data, err := os.ReadFile(filepath.Join(root, "eula.txt"))
	return err == nil && acceptedEULA.Match(data)
}

// Apply node-owned network settings after verified pack overrides are staged.
func ConfigureServer(stage string, port int, eulaApproved bool) error {
	if port < 1 || port > 65535 {
		return fmt.Errorf("invalid Cell game port")
	}
	target := filepath.Join(stage, "server.properties")
	data, err := os.ReadFile(target)
	if err != nil && !os.IsNotExist(err) {
		return err
	}
	text := string(data)
	line := fmt.Sprintf("server-port=%d", port)
	if serverPortLine.MatchString(text) {
		text = serverPortLine.ReplaceAllString(text, line)
	} else {
		text = strings.TrimRight(text, "\r\n") + "\n" + line + "\n"
	}
	if err := os.WriteFile(target, []byte(text), 0640); err != nil {
		return err
	}
	if _, err := os.Stat(filepath.Join(stage, "eula.txt")); os.IsNotExist(err) {
		value := "false"
		if eulaApproved {
			value = "true"
		}
		return os.WriteFile(filepath.Join(stage, "eula.txt"), []byte("eula="+value+"\n"), 0640)
	} else {
		return err
	}
}
