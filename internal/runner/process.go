package runner

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/creack/pty"
)

type processExit struct {
	ExitCode   *int
	Signal     string
	FinishedAt time.Time
}

type agentProcess struct {
	mu        sync.Mutex
	writeMu   sync.Mutex
	command   []string
	directory string
	environ   []string
	logger    *slog.Logger
	output    func([]byte)
	exited    func(processExit)

	cmd       *exec.Cmd
	terminal  *os.File
	startedAt time.Time
	exit      *processExit
	done      chan struct{}
	stopOnce  sync.Once
}

func newAgentProcess(command []string, directory string, environ []string, logger *slog.Logger, output func([]byte), exited func(processExit)) *agentProcess {
	return &agentProcess{
		command:   append([]string(nil), command...),
		directory: directory,
		environ:   append([]string(nil), environ...),
		logger:    logger,
		output:    output,
		exited:    exited,
		done:      make(chan struct{}),
	}
}

func (p *agentProcess) start(size TerminalSize) error {
	if len(p.command) == 0 {
		return fmt.Errorf("agent command is required")
	}
	cmd := exec.Command(p.command[0], p.command[1:]...)
	cmd.Dir = p.directory
	cmd.Env = withTerminalEnvironment(p.environ)
	terminal, err := pty.StartWithSize(cmd, &pty.Winsize{
		Cols: size.Columns,
		Rows: size.Rows,
		X:    size.PixelWidth,
		Y:    size.PixelHeight,
	})
	if err != nil {
		return fmt.Errorf("start agent PTY: %w", err)
	}

	p.mu.Lock()
	p.cmd = cmd
	p.terminal = terminal
	p.startedAt = time.Now().UTC()
	p.mu.Unlock()
	p.logger.Info("agent process started", "pid", cmd.Process.Pid)

	readDone := make(chan struct{})
	go p.readOutput(terminal, readDone)
	go p.wait(cmd, terminal, readDone)
	return nil
}

func withTerminalEnvironment(environ []string) []string {
	result := make([]string, 0, len(environ)+1)
	found := false
	for _, item := range environ {
		if strings.HasPrefix(item, "TERM=") {
			result = append(result, "TERM=xterm-256color")
			found = true
			continue
		}
		result = append(result, item)
	}
	if !found {
		result = append(result, "TERM=xterm-256color")
	}
	return result
}

func (p *agentProcess) readOutput(terminal *os.File, done chan<- struct{}) {
	defer close(done)
	buffer := make([]byte, 32*1024)
	for {
		count, err := terminal.Read(buffer)
		if count > 0 {
			p.output(buffer[:count])
		}
		if err != nil {
			if !errors.Is(err, io.EOF) && !errors.Is(err, syscall.EIO) && !errors.Is(err, os.ErrClosed) {
				p.logger.Warn("PTY read ended", "error", err)
			}
			return
		}
	}
}

func (p *agentProcess) wait(cmd *exec.Cmd, terminal *os.File, readDone <-chan struct{}) {
	err := cmd.Wait()
	select {
	case <-readDone:
	case <-time.After(250 * time.Millisecond):
		_ = terminal.Close()
		<-readDone
	}
	_ = terminal.Close()

	exit := exitFromWait(err)
	p.mu.Lock()
	p.exit = &exit
	p.mu.Unlock()
	p.logger.Info("agent process exited", "exit_code", exit.ExitCode, "signal", exit.Signal)
	p.exited(exit)
	close(p.done)
}

func exitFromWait(err error) processExit {
	result := processExit{FinishedAt: time.Now().UTC()}
	if err == nil {
		code := 0
		result.ExitCode = &code
		return result
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		if status, ok := exitError.Sys().(syscall.WaitStatus); ok && status.Signaled() {
			result.Signal = signalName(status.Signal())
			return result
		}
		code := exitError.ExitCode()
		if code < 0 {
			code = 255
		}
		result.ExitCode = &code
		return result
	}
	code := 255
	result.ExitCode = &code
	return result
}

func signalName(signal syscall.Signal) string {
	names := map[syscall.Signal]string{
		syscall.SIGHUP:  "SIGHUP",
		syscall.SIGINT:  "SIGINT",
		syscall.SIGQUIT: "SIGQUIT",
		syscall.SIGKILL: "SIGKILL",
		syscall.SIGTERM: "SIGTERM",
		syscall.SIGUSR1: "SIGUSR1",
		syscall.SIGUSR2: "SIGUSR2",
	}
	if name, ok := names[signal]; ok {
		return name
	}
	return fmt.Sprintf("SIG%d", signal)
}

func parseSignal(name string) (syscall.Signal, error) {
	signals := map[string]syscall.Signal{
		"SIGHUP":  syscall.SIGHUP,
		"SIGINT":  syscall.SIGINT,
		"SIGQUIT": syscall.SIGQUIT,
		"SIGTERM": syscall.SIGTERM,
		"SIGUSR1": syscall.SIGUSR1,
		"SIGUSR2": syscall.SIGUSR2,
	}
	signal, ok := signals[strings.ToUpper(name)]
	if !ok {
		return 0, fmt.Errorf("unsupported signal %q", name)
	}
	return signal, nil
}

func (p *agentProcess) write(data []byte) error {
	p.writeMu.Lock()
	defer p.writeMu.Unlock()
	p.mu.Lock()
	terminal := p.terminal
	exited := p.exit != nil
	p.mu.Unlock()
	if terminal == nil || exited {
		return fmt.Errorf("agent is not running")
	}
	_, err := terminal.Write(data)
	return err
}

func (p *agentProcess) resize(size TerminalSize) error {
	if err := size.Validate(); err != nil {
		return err
	}
	p.mu.Lock()
	terminal := p.terminal
	exited := p.exit != nil
	p.mu.Unlock()
	if terminal == nil || exited {
		return fmt.Errorf("agent is not running")
	}
	return pty.Setsize(terminal, &pty.Winsize{Cols: size.Columns, Rows: size.Rows, X: size.PixelWidth, Y: size.PixelHeight})
}

func (p *agentProcess) signal(signal syscall.Signal) error {
	p.mu.Lock()
	cmd := p.cmd
	exited := p.exit != nil
	p.mu.Unlock()
	if cmd == nil || exited {
		return fmt.Errorf("agent is not running")
	}
	return syscall.Kill(-cmd.Process.Pid, signal)
}

func (p *agentProcess) terminate(grace time.Duration) {
	p.stopOnce.Do(func() {
		if err := p.signal(syscall.SIGTERM); err != nil {
			return
		}
		timer := time.NewTimer(grace)
		defer timer.Stop()
		select {
		case <-p.done:
		case <-timer.C:
			p.logger.Warn("agent did not exit within grace period; forcing termination", "grace", grace)
			_ = p.signal(syscall.SIGKILL)
			<-p.done
		}
	})
}

func (p *agentProcess) snapshot() (pid int, startedAt time.Time, exit *processExit) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.cmd != nil && p.cmd.Process != nil {
		pid = p.cmd.Process.Pid
	}
	if p.exit != nil {
		copyOfExit := *p.exit
		exit = &copyOfExit
	}
	return pid, p.startedAt, exit
}
