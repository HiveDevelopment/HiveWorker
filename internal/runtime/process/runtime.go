package process

import (
	"bufio"
	"errors"
	"io"
	"os/exec"
	"runtime"
	"sync"
	"time"

	hiveruntime "hivepanel-worker/internal/runtime"

	ps "github.com/shirou/gopsutil/v4/process"
)

type ProcessRuntime struct {
	mutex     sync.Mutex
	processes map[string]*RunningProcess
}

type RunningProcess struct {
	cmd       *exec.Cmd
	stdin     io.WriteCloser
	startedAt time.Time
	done      chan struct{}
}

func New() *ProcessRuntime {
	return &ProcessRuntime{
		processes: map[string]*RunningProcess{},
	}
}

func (r *ProcessRuntime) Start(cell hiveruntime.RuntimeCell, onOutput func(line string), onExit func()) error {
	r.mutex.Lock()

	if _, exists := r.processes[cell.ID]; exists {
		r.mutex.Unlock()
		return errors.New("cell already running")
	}

	cmd := shellCommand(cell.Command)
	cmd.Dir = cell.InstanceDir

	stdin, err := cmd.StdinPipe()
	if err != nil {
		r.mutex.Unlock()
		return err
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		r.mutex.Unlock()
		return err
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		r.mutex.Unlock()
		return err
	}

	configureProcessGroup(cmd)
	if err := cmd.Start(); err != nil {
		r.mutex.Unlock()
		return err
	}

	done := make(chan struct{})
	r.processes[cell.ID] = &RunningProcess{
		done:      done,
		cmd:       cmd,
		stdin:     stdin,
		startedAt: time.Now(),
	}

	r.mutex.Unlock()

	go capture(stdout, onOutput)
	go capture(stderr, onOutput)

	go func() {
		_ = cmd.Wait()
		close(done)

		r.mutex.Lock()
		delete(r.processes, cell.ID)
		r.mutex.Unlock()

		onExit()
	}()

	return nil
}

func (r *ProcessRuntime) Stop(id string) error {
	return r.StopConfigured(id, hiveruntime.StopOptions{Timeout: 30, Signal: "SIGTERM"})
}

func (r *ProcessRuntime) StopConfigured(id string, options hiveruntime.StopOptions) error {
	r.mutex.Lock()
	running, exists := r.processes[id]
	r.mutex.Unlock()
	if !exists {
		return errors.New("cell is not running")
	}
	seconds := options.Timeout
	if seconds <= 0 {
		seconds = 30
	}
	if seconds > 600 {
		seconds = 600
	}
	wait := func() bool {
		timer := time.NewTimer(time.Duration(seconds) * time.Second)
		defer timer.Stop()
		select {
		case <-running.done:
			return true
		case <-timer.C:
			return false
		}
	}
	if options.Command != "" && r.SendCommand(id, options.Command) == nil {
		if wait() {
			return nil
		}
	}
	select {
	case <-running.done:
		return nil
	default:
	}
	if err := signalProcess(running.cmd, options.Signal, false); err != nil {
		return err
	}
	if wait() {
		return nil
	}
	return signalProcess(running.cmd, "", true)
}

func (r *ProcessRuntime) SendCommand(id string, command string) error {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	running, exists := r.processes[id]
	if !exists {
		return errors.New("cell is not running")
	}

	_, err := io.WriteString(running.stdin, command+"\n")
	return err
}

func (r *ProcessRuntime) Stats(id string) (*hiveruntime.Stats, error) {
	r.mutex.Lock()
	running, exists := r.processes[id]
	if !exists {
		r.mutex.Unlock()
		return &hiveruntime.Stats{}, nil
	}

	pid := running.cmd.Process.Pid
	r.mutex.Unlock()

	stats := &hiveruntime.Stats{
		PID: pid,
	}

	proc, err := ps.NewProcess(int32(pid))
	if err == nil {
		cpu, err := proc.CPUPercent()
		if err == nil {
			stats.CPU = cpu
		}

		mem, err := proc.MemoryInfo()
		if err == nil && mem != nil {
			stats.MemoryMB = float64(mem.RSS) / 1024 / 1024
		}
	}

	return stats, nil
}

func (r *ProcessRuntime) IsRunning(id string) bool {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	_, exists := r.processes[id]
	return exists
}

func (r *ProcessRuntime) StartedAt(id string) *time.Time {
	r.mutex.Lock()
	defer r.mutex.Unlock()

	running, exists := r.processes[id]
	if !exists {
		return nil
	}

	startedAt := running.startedAt
	return &startedAt
}

func capture(pipe io.ReadCloser, onOutput func(line string)) {
	scanner := bufio.NewScanner(pipe)

	for scanner.Scan() {
		onOutput(scanner.Text())
	}
}

func shellCommand(command string) *exec.Cmd {
	if runtime.GOOS == "windows" {
		return exec.Command("powershell", "-Command", command)
	}

	return exec.Command("bash", "-lc", command)
}
