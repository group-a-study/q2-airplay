package main

import (
	"bytes"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-airplay/internal/q2"
)

// A troubleshooting log, while dropouts are chased (the user allowed one on the Q2): the engine's
// steps, cliairplay's own lines and a heartbeat of what the Q2 does, buffered and appended to the card
// every few seconds, opened and closed each time so the card is never kept busy; 2 MB, then one older
// file. A nil log logs nothing.

const (
	logMax     = 2 << 20
	logEvery   = 5 * time.Second
	logPerSec  = 200 // lines a second at most; the rest counted, not written
	heartEvery = 2 * time.Second
)

type debugLog struct {
	path    string
	mu      sync.Mutex
	buf     bytes.Buffer
	sec     int64
	n       int
	dropped int
}

var dbg *debugLog

func openLog(path string) *debugLog {
	if path == "" {
		return nil
	}
	l := &debugLog{path: path}
	go func() {
		for range time.Tick(logEvery) {
			l.flush()
		}
	}()
	return l
}

func (l *debugLog) printf(format string, args ...any) {
	if l == nil {
		return
	}
	now := time.Now()
	l.mu.Lock()
	defer l.mu.Unlock()
	if s := now.Unix(); s != l.sec {
		if l.dropped > 0 {
			fmt.Fprintf(&l.buf, "%s (%d lines left out)\n", now.Format("15:04:05.000"), l.dropped)
		}
		l.sec, l.n, l.dropped = s, 0, 0
	}
	if l.n++; l.n > logPerSec {
		l.dropped++
		return
	}
	fmt.Fprintf(&l.buf, "%s %d ", now.Format("15:04:05.000"), q2.MonoNS()/1e6)
	fmt.Fprintf(&l.buf, format, args...)
	l.buf.WriteByte('\n')
}

// flush appends what is buffered; a card away (USB storage) keeps it for the next try, up to a cap.
func (l *debugLog) flush() {
	if l == nil {
		return
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.buf.Len() == 0 {
		return
	}
	if st, err := os.Stat(l.path); err == nil && st.Size() > logMax {
		os.Rename(l.path, l.path+".1")
	}
	f, err := os.OpenFile(l.path, os.O_WRONLY|os.O_CREATE|os.O_APPEND, 0o644)
	if err == nil {
		_, err = f.Write(l.buf.Bytes())
		f.Close()
	}
	if err == nil || l.buf.Len() > logMax/4 {
		l.buf.Reset()
	}
}

// probe is the heartbeat's view of the Q2: what the kernel says of its audio devices, load, Wi-Fi
// and the CPU time of the player, this app and cliairplay since the last.
type probe struct {
	last    time.Time
	ticks   map[int]int64
	beat    uint32
	player  int
	scanned time.Time
}

func (p *probe) line(now time.Time, t *tap, sender int) string {
	var b strings.Builder
	beat := t.u32(offBeat)
	dt := now.Sub(p.last).Seconds()
	if p.last.IsZero() || dt <= 0 {
		dt = 1
	}
	fmt.Fprintf(&b, "ui=%.0f/s", float64(beat-p.beat)/dt) // demo's passes: ~10/s dark, more lit
	p.beat = beat
	if now.Sub(p.scanned) > 30*time.Second || p.player == 0 {
		p.player, p.scanned = pidOf("hciplayer"), now
	}
	if p.ticks == nil {
		p.ticks = map[int]int64{}
	}
	for _, c := range []struct {
		name string
		pid  int
	}{{"player", p.player}, {"app", os.Getpid()}, {"sender", sender}} {
		if c.pid <= 0 {
			continue
		}
		ticks := cpuTicks(c.pid)
		if old, ok := p.ticks[c.pid]; ok && ticks >= old {
			fmt.Fprintf(&b, " %s=%.0f%%", c.name, float64(ticks-old)/dt) // 100 ticks a second
		}
		p.ticks[c.pid] = ticks
	}
	p.last = now
	if l, err := os.ReadFile("/proc/loadavg"); err == nil {
		fmt.Fprintf(&b, " load=%s", strings.Fields(string(l))[0])
	}
	if c, err := os.ReadFile("/sys/devices/system/cpu/online"); err == nil {
		fmt.Fprintf(&b, " cpus=%s", strings.TrimSpace(string(c)))
	}
	if w, err := os.ReadFile("/proc/net/rtl8733bs/wlan0/rx_signal"); err == nil {
		fmt.Fprintf(&b, " wifi=%s", strings.Join(strings.Fields(string(w)), ""))
	}
	pcms, _ := filepath.Glob("/proc/asound/card*/pcm*p/sub*/status")
	for _, f := range pcms {
		s, err := os.ReadFile(f)
		if err != nil || strings.TrimSpace(string(s)) == "closed" {
			continue
		}
		kv := map[string]string{}
		for _, line := range strings.Split(string(s), "\n") {
			if k, v, ok := strings.Cut(line, ":"); ok {
				kv[strings.TrimSpace(k)] = strings.TrimSpace(v)
			}
		}
		name := strings.TrimPrefix(filepath.Dir(f), "/proc/asound/")
		fmt.Fprintf(&b, " %s=%s,delay=%s,avail=%s,hw=%s,appl=%s", name, kv["state"], kv["delay"],
			kv["avail"], kv["hw_ptr"], kv["appl_ptr"])
	}
	return b.String()
}

func cpuTicks(pid int) int64 {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	s := string(b)
	if i := strings.LastIndexByte(s, ')'); i >= 0 {
		s = s[i+1:]
	}
	f := strings.Fields(s) // from field 3: state; utime and stime are fields 14, 15
	if len(f) < 13 {
		return 0
	}
	u, _ := strconv.ParseInt(f[11], 10, 64)
	st, _ := strconv.ParseInt(f[12], 10, 64)
	return u + st
}

func pidOf(comm string) int {
	dirs, _ := os.ReadDir("/proc")
	for _, d := range dirs {
		pid, err := strconv.Atoi(d.Name())
		if err != nil {
			continue
		}
		if c, err := os.ReadFile("/proc/" + d.Name() + "/comm"); err == nil && strings.TrimSpace(string(c)) == comm {
			return pid
		}
	}
	return 0
}
