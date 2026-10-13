package install

import (
	"bytes"
	"os/exec"
	"strings"
)

type logWriter struct {
	pending []byte
	tail    []byte
	log     Logger
}

func (w *logWriter) Write(data []byte) (int, error) {
	size := len(data)
	w.tail = append(w.tail, data...)
	if len(w.tail) > 65536 {
		w.tail = w.tail[len(w.tail)-65536:]
	}
	for len(data) > 0 {
		end := bytes.IndexAny(data, "\r\n")
		if end < 0 {
			end = len(data)
		}
		// Flush long lines in bounded chunks too.
		take := end
		if take > 8192-len(w.pending) {
			take = 8192 - len(w.pending)
		}
		w.pending = append(w.pending, data[:take]...)
		data = data[take:]
		if len(w.pending) == 8192 || (len(data) > 0 && (data[0] == '\r' || data[0] == '\n')) {
			w.flush()
			if len(data) > 0 && (data[0] == '\r' || data[0] == '\n') {
				data = data[1:]
			}
		}
	}
	return size, nil
}
func (w *logWriter) flush() {
	if line := strings.TrimSpace(string(w.pending)); line != "" && w.log != nil {
		w.log(line)
	}
	w.pending = nil
}
func runLogged(cmd *exec.Cmd, log Logger) ([]byte, error) {
	writer := &logWriter{log: log}
	// A shared comparable writer makes os/exec serialize stdout and stderr writes.
	cmd.Stdout, cmd.Stderr = writer, writer
	err := cmd.Run()
	writer.flush()
	return writer.tail, err
}
