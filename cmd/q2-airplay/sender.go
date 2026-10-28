package main

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// A sender is one cliairplay (music-assistant/airplay-cli, built for the Q2: contrib/shanlingq2) for
// one speaker: 16-bit stereo at tapRate on its stdin, KEY=VALUE commands on its command pipe, and its
// "[STATUS] …" lines, on stdout and stderr both, as events. Its other output is dropped.

type event struct {
	kind string            // "connected", "started", "flushed", "clock_ready", "error", "exit", …
	kv   map[string]string // the line's key=value fields
}

type sender struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	pipe   *os.File
	events chan event
	audio  chan []byte
	idle   chan struct{} // receives once the audio queued so far is written
	mu     sync.Mutex
	exited bool
}

type senderOptions struct {
	bin, cmdpipe, dacp, remote, name string
	protocol                         string // auto, or airplay2-compat for the fallback
	volume, latencyMS                int
	sp                               speaker
}

func startSender(o senderOptions) (*sender, error) {
	os.Remove(o.cmdpipe)
	if err := syscall.Mkfifo(o.cmdpipe, 0o600); err != nil {
		return nil, err
	}
	args := []string{"--protocol", o.protocol, "--port", strconv.Itoa(o.sp.Port),
		"--volume", strconv.Itoa(o.volume), "--dacp", o.dacp, "--activeremote", o.remote,
		"--name", o.name, "--hostname", "shanling-q2", "--debug", "1", "--cmdpipe", o.cmdpipe}
	if o.latencyMS > 0 {
		args = append(args, "--latency", strconv.Itoa(o.latencyMS))
	}
	if o.sp.TXT != "" {
		args = append(args, "--txt", o.sp.TXT)
	}
	for _, k := range []string{"et", "md", "am", "pk", "pw", "cn"} { // _raop's TXT, for AirPlay 1
		if v := o.sp.RaopTXT[k]; v != "" {
			args = append(args, "--"+k, v)
		}
	}
	args = append(args, o.sp.IP)
	cmd := exec.Command(o.bin, args...)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return nil, err
	}
	// Read and write: the open never waits for cliairplay's end, and a write never fails for want of
	// a reader before it opens its own.
	pipe, err := os.OpenFile(o.cmdpipe, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	if err := cmd.Start(); err != nil {
		pipe.Close()
		return nil, err
	}
	s := &sender{cmd: cmd, stdin: stdin, pipe: pipe, events: make(chan event, 64),
		audio: make(chan []byte, 64), idle: make(chan struct{}, 1)}
	var wg sync.WaitGroup
	wg.Add(2)
	go func() { defer wg.Done(); s.scan(stdout) }()
	go func() { defer wg.Done(); s.scan(stderr) }()
	go func() {
		wg.Wait()
		err := cmd.Wait()
		code := 0
		if err != nil {
			code = -1
			if ee, ok := err.(*exec.ExitError); ok {
				code = ee.ExitCode()
			}
		}
		s.mu.Lock()
		s.exited = true
		s.mu.Unlock()
		pipe.Close()
		s.events <- event{"exit", map[string]string{"code": strconv.Itoa(code)}}
	}()
	go s.write()
	return s, nil
}

// write sends the queued audio to cliairplay's stdin, in order; a nil chunk asks for idle.
func (s *sender) write() {
	for b := range s.audio {
		if b == nil {
			select {
			case s.idle <- struct{}{}:
			default:
			}
			continue
		}
		if _, err := s.stdin.Write(b); err != nil {
			for range s.audio { // gone: drain until closed
			}
			return
		}
	}
	s.stdin.Close()
}

// send queues audio; false when too much is queued already (cliairplay isn't reading).
func (s *sender) send(b []byte) bool {
	select {
	case s.audio <- b:
		return true
	default:
		return false
	}
}

// drained waits (up to d) until everything queued so far is written.
func (s *sender) drained(d time.Duration) bool {
	select {
	case <-s.idle:
	default:
	}
	if !s.send(nil) {
		return false
	}
	select {
	case <-s.idle:
		return true
	case <-time.After(d):
		return false
	}
}

// command writes KEY=VALUE lines, then the action, in one write.
func (s *sender) command(lines ...string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.exited {
		return
	}
	s.pipe.Write([]byte(strings.Join(lines, "\n") + "\n"))
}

// stop asks it to end (ACTION=STOP), and kills it if it hasn't within a few seconds.
func (s *sender) stop() {
	s.command("ACTION=STOP")
	close(s.audio)
	go func() {
		time.Sleep(4 * time.Second)
		s.mu.Lock()
		gone := s.exited
		s.mu.Unlock()
		if !gone {
			syscall.Kill(-s.cmd.Process.Pid, syscall.SIGKILL)
		}
	}()
}

func (s *sender) scan(r io.Reader) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 4096), 64<<10)
	for sc.Scan() {
		if e, ok := parseStatus(sc.Text()); ok {
			select {
			case s.events <- e:
			default: // the engine is behind: a status line it would read late anyway
			}
		}
	}
	io.Copy(io.Discard, r)
}

// parseStatus reads "[STATUS] kind k=v k=\"v w\"…" (and [ERROR] lines, as kind "fatal").
func parseStatus(line string) (event, bool) {
	var rest string
	switch {
	case strings.HasPrefix(line, "[STATUS] "):
		rest = line[len("[STATUS] "):]
	case strings.HasPrefix(line, "[ERROR] "):
		return event{"fatal", map[string]string{"detail": strings.TrimSpace(line[len("[ERROR] "):])}}, true
	default:
		return event{}, false
	}
	e := event{kv: map[string]string{}}
	for {
		rest = strings.TrimLeft(rest, " ")
		if rest == "" {
			break
		}
		end := strings.IndexAny(rest, " =")
		if end < 0 {
			end = len(rest)
		}
		word := rest[:end]
		rest = rest[end:]
		if !strings.HasPrefix(rest, "=") {
			if e.kind == "" {
				e.kind = word
			}
			continue
		}
		rest = rest[1:]
		var v string
		if strings.HasPrefix(rest, `"`) {
			q := strings.IndexByte(rest[1:], '"')
			if q < 0 {
				q = len(rest) - 1
			}
			v, rest = rest[1:1+q], rest[min(len(rest), q+2):]
		} else {
			sp := strings.IndexByte(rest, ' ')
			if sp < 0 {
				sp = len(rest)
			}
			v, rest = rest[:sp], rest[sp:]
		}
		e.kv[word] = v
	}
	return e, e.kind != ""
}

func (e event) int64(k string) int64 {
	v, _ := strconv.ParseInt(e.kv[k], 10, 64)
	return v
}

func (e event) String() string { return fmt.Sprintf("%s %v", e.kind, e.kv) }
