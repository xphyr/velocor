// Package kernel is the spike of Velocor's kernel data plane (docs/design/kernel-dataplane.md,
// TODO.md K-1): md RAID for redundancy, LVM thin pools for volumes and snapshots, a dm-linear
// wrapper per volume (the quiesce point for group snapshots), and the kernel NVMe/TCP target
// configured through configfs.
//
// It is not wired into velod yet. Its API is the candidate for velod's data-plane interface in
// K-2, shaped by what the spike's integration tests (integration_test.go, build tag
// kernelintegration, run as root) show about the real tools.
package kernel

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os/exec"
	"strings"
	"sync"
)

// Runner runs one host command and returns its standard output. A non-zero exit is an error
// that carries the command's standard error.
type Runner interface {
	Run(ctx context.Context, name string, args ...string) ([]byte, error)
}

// CommandError is a failed command: what ran, its exit code and what it printed on stderr.
type CommandError struct {
	Cmd    string
	Code   int
	Stderr string
	Err    error
}

func (e *CommandError) Error() string {
	msg := strings.TrimSpace(e.Stderr)
	if msg == "" && e.Err != nil {
		msg = e.Err.Error()
	}
	return fmt.Sprintf("%s: exit %d: %s", e.Cmd, e.Code, msg)
}

func (e *CommandError) Unwrap() error { return e.Err }

// Exec runs commands on this host. With HostNamespace set, it runs them in PID 1's mount and
// IPC namespaces through nsenter, for a velod in a container that shares the host's PID namespace:
// the storage tools, their configuration and /dev are the host's.
type Exec struct {
	HostNamespace bool
}

// Streamer is a Runner that can hand a command's output to fn as it comes, for output that can be
// large.
type Streamer interface {
	Stream(ctx context.Context, fn func(io.Reader) error, name string, args ...string) error
}

// stream runs a command and hands its output to fn: as it comes if the Runner can.
func (p *Plane) stream(ctx context.Context, fn func(io.Reader) error, name string, args ...string) error {
	if s, ok := p.Run.(Streamer); ok {
		return s.Stream(ctx, fn, name, args...)
	}
	out, err := p.Run.Run(ctx, name, args...)
	if err != nil {
		return err
	}
	return fn(bytes.NewReader(out))
}

// command is the command as this host runs it.
func (x Exec) command(name string, args []string) (string, []string) {
	if x.HostNamespace {
		// The IPC namespace too: LVM and dmsetup wait for udev through SysV semaphores, which
		// udev on the host completes in the host's IPC namespace (in velod's own, they wait
		// forever: found on the lab appliance, lvcreate hung creating a thin pool).
		args = append([]string{"-t", "1", "-m", "-i", "--", name}, args...)
		name = "nsenter"
	}
	return name, args
}

// Stream implements Streamer. fn's error wins over the command's, which fn stopping early causes.
func (x Exec) Stream(ctx context.Context, fn func(io.Reader) error, name string, args ...string) error {
	name, args = x.command(name, args)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd := exec.CommandContext(ctx, name, args...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	out, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	if err := cmd.Start(); err != nil {
		return err
	}
	ferr := fn(out)
	if ferr != nil {
		cancel()
	} else {
		_, _ = io.Copy(io.Discard, out)
	}
	if err := cmd.Wait(); err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		cerr := &CommandError{Cmd: name + " " + strings.Join(args, " "), Code: code, Stderr: stderr.String(), Err: err}
		if ferr == nil || stderr.Len() > 0 { // the output broke off because the command failed
			return errors.Join(cerr, ferr)
		}
	}
	return ferr
}

// Run implements Runner.
func (x Exec) Run(ctx context.Context, name string, args ...string) ([]byte, error) {
	name, args = x.command(name, args)
	cmd := exec.CommandContext(ctx, name, args...)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	if err := cmd.Run(); err != nil {
		code := -1
		var exit *exec.ExitError
		if errors.As(err, &exit) {
			code = exit.ExitCode()
		}
		return stdout.Bytes(), &CommandError{Cmd: name + " " + strings.Join(args, " "), Code: code, Stderr: stderr.String(), Err: err}
	}
	return stdout.Bytes(), nil
}

// Script is a Runner for unit tests: it answers commands from a list of expectations, in order,
// and records every call.
type Script struct {
	mu    sync.Mutex
	Steps []Step
	Calls []string
}

// Step is one expected command (matched by prefix of "name arg1 arg2 ...") and its answer.
type Step struct {
	Prefix string
	Out    string
	Err    error
}

// Run implements Runner.
func (s *Script) Run(_ context.Context, name string, args ...string) ([]byte, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	line := strings.TrimSpace(name + " " + strings.Join(args, " "))
	s.Calls = append(s.Calls, line)
	if len(s.Steps) == 0 {
		return nil, fmt.Errorf("unexpected command %q", line)
	}
	st := s.Steps[0]
	if !strings.HasPrefix(line, st.Prefix) {
		return nil, fmt.Errorf("command %q, expected one starting %q", line, st.Prefix)
	}
	s.Steps = s.Steps[1:]
	return []byte(st.Out), st.Err
}
