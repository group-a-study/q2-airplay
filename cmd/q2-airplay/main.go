// q2-airplay is a Q2 Pod card app (contrib/shanlingq2/README): it plays what the Q2 plays on an AirPlay
// speaker (Sonos, HomePod, Apple TV, AirPlay 1 speakers), chosen on the page Now Playing's AirPlay
// button opens. Q2 Pod's hciplayer filter hands it the audio (tap.go); cliairplay, Music Assistant's
// AirPlay sender built for the Q2, sends it on (sender.go); the speaker plays it a moment behind the
// Q2, whose own output is silent meanwhile (cast.go). It runs from the card's .airplay folder, copied
// to /tmp first, so the card is never kept busy, and draws its page over Q2SPOT/1.
package main

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"flag"
	"fmt"
	"math/big"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/group-a-study/q2-airplay/internal/q2"
	"github.com/group-a-study/q2-airplay/internal/q2spot"
)

const version = "q2-v1.1"

// settings is .airplay/config.json.
type settings struct {
	DACP    string `json:"dacp"`     // this sender's AirPlay identity, kept so a speaker knows it again
	Remote  string `json:"remote"`   // its Active-Remote id
	DelayMS int    `json:"delay_ms"` // how far behind the Q2 the speaker plays
}

var delays = []int{750, 1000, 1500, 2000, 3000}

const (
	idleExit    = 10 * time.Minute // nothing chosen and the page unused this long: the app ends
	lookFast    = 2 * time.Second  // queries this often for a while after the page opens
	lookSlow    = 15 * time.Second // then this often while the page is used or a speaker plays
	lookBurst   = 12 * time.Second
	pageUsedFor = 5 * time.Second // a request this recent: the page is open
)

type app struct {
	dir     string
	mu      sync.Mutex
	cfg     settings
	eng     *engine
	br      *browser
	opened  time.Time // the page's first request after a quiet spell
	asked   time.Time // its last request
	looked  time.Time
	started time.Time
}

func main() {
	dir := flag.String("dir", "/mnt/mmc/.airplay", "the app's card folder")
	bin := flag.String("sender", "/tmp/q2-airplay.cliairplay", "cliairplay")
	browse := flag.String("browse", "/tmp/q2-airplay.browse.sock", "the Q2SPOT/1 socket")
	statusFile := flag.String("status", "/tmp/q2-airplay.state", "the status file Q2 Pod reads")
	showVersion := flag.Bool("version", false, "print the version")
	flag.Parse()
	if *showVersion {
		fmt.Println(version)
		return
	}
	a := &app{dir: *dir, started: time.Now()}
	a.load()
	t, err := openTap(tapFile)
	if err != nil {
		fmt.Fprintln(os.Stderr, "q2-airplay:", err)
		os.Exit(1)
	}
	if a.br, err = newBrowser(); err != nil {
		fmt.Fprintln(os.Stderr, "q2-airplay:", err)
		os.Exit(1)
	}
	a.eng = newEngine(t, engineOptions{bin: *bin, cmdpipe: "/tmp/q2-airplay.cmd", dacp: a.cfg.DACP,
		remote: a.cfg.Remote, artFile: "/tmp/q2-airplay-art", track: trackFile, config: configFile,
		delayMS: a.delay})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { a.eng.run(stop); close(done) }()
	go q2spot.Serve(*browse, a.handle)
	state := &q2.State{Path: *statusFile}
	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	tick := time.NewTicker(time.Second)
	code := 0
loop:
	for {
		select {
		case <-sig:
			break loop
		case now := <-tick.C:
			a.discover(now)
			playing := a.eng.active() && t.status() == playPlaying
			hold := "0"
			if playing {
				hold = "1"
			}
			state.Write(map[string]string{"hold": hold, "version": version})
			a.mu.Lock()
			idle := !a.eng.active() && now.Sub(a.asked) > idleExit && now.Sub(a.started) > idleExit
			a.mu.Unlock()
			if idle {
				break loop
			}
		}
	}
	close(stop)
	<-done
	state.Remove()
	os.Remove(*browse)
	os.Exit(code)
}

func (a *app) delay() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.cfg.DelayMS
}

// load reads the settings, making the identity the first time.
func (a *app) load() {
	path := filepath.Join(a.dir, "config.json")
	if b, err := os.ReadFile(path); err == nil {
		json.Unmarshal(b, &a.cfg)
	}
	changed := false
	if len(a.cfg.DACP) != 16 {
		b := make([]byte, 8)
		rand.Read(b)
		a.cfg.DACP, changed = strings.ToUpper(hex.EncodeToString(b)), true
	}
	if a.cfg.Remote == "" {
		n, _ := rand.Int(rand.Reader, big.NewInt(1<<31))
		a.cfg.Remote, changed = n.String(), true
	}
	valid := false
	for _, d := range delays {
		valid = valid || d == a.cfg.DelayMS
	}
	if !valid {
		a.cfg.DelayMS, changed = 1000, true
	}
	if changed {
		a.save()
	}
}

func (a *app) save() {
	b, _ := json.MarshalIndent(a.cfg, "", "  ")
	q2.WriteFile(filepath.Join(a.dir, "config.json"), append(b, '\n'))
}

// discover asks for speakers while the page is open: often just after it opens, then now and again;
// not at all otherwise (a chosen speaker is kept as it was found).
func (a *app) discover(now time.Time) {
	a.mu.Lock()
	open := now.Sub(a.asked) < pageUsedFor
	fast := open && now.Sub(a.opened) < lookBurst
	a.mu.Unlock()
	every := lookSlow
	if fast {
		every = lookFast
	}
	if open && now.Sub(a.looked) >= every {
		a.looked = now
		a.br.query()
	}
}

func (a *app) handle(r q2spot.Request) q2spot.Reply {
	now := time.Now()
	a.mu.Lock()
	if now.Sub(a.asked) > pageUsedFor {
		a.opened = now
		a.looked = time.Time{} // look at once
	}
	a.asked = now
	a.mu.Unlock()
	switch r.Op {
	case "menu":
		return a.menu(now)
	case "cast":
		for _, sp := range a.br.speakers(now) {
			if sp.key() == r.Arg(0) {
				sp := sp
				a.eng.choose(&sp)
				return q2spot.Reply{}
			}
		}
		return q2spot.Fail(q2spot.Unavailable, "That speaker isn't there any more")
	case "local":
		a.eng.choose(nil)
		return q2spot.Reply{}
	case "delay":
		a.mu.Lock()
		next := delays[0]
		for i, d := range delays {
			if d == a.cfg.DelayMS && i+1 < len(delays) {
				next = delays[i+1]
			}
		}
		a.cfg.DelayMS = next
		a.save()
		a.mu.Unlock()
		msg := ""
		if a.eng.active() {
			msg = "The new delay applies from the next song or pause"
		}
		return q2spot.Reply{Message: msg}
	case "caps":
		return q2spot.Reply{Message: version}
	}
	return q2spot.Fail(q2spot.Unsupported, "Unknown request")
}

func (a *app) menu(now time.Time) q2spot.Reply {
	chosen, line, playing := a.eng.state()
	rows := []q2spot.Row{}
	if line != "" {
		rows = append(rows, q2spot.Row{Kind: "info", Title: line})
	}
	here := ""
	if chosen == "" {
		here = "✓ Playing on the Q2"
	}
	rows = append(rows, q2spot.Row{Kind: "action", URI: "local", Title: "This Q2", Subtitle: here})
	speakers := a.br.speakers(now)
	for _, sp := range speakers {
		sub := maker(sp.Model + " " + sp.TXT)
		if sp.key() == chosen {
			sub = "✓ Connecting…"
			if playing {
				sub = "✓ Playing"
			}
		}
		rows = append(rows, q2spot.Row{Kind: "action", URI: "cast", Title: sp.Name, Subtitle: sub, Context: sp.key()})
	}
	if len(speakers) == 0 {
		a.mu.Lock()
		looking := now.Sub(a.opened) < lookBurst
		a.mu.Unlock()
		t := "No AirPlay speakers on this Wi-Fi"
		if looking {
			t = "Looking for speakers…"
		}
		rows = append(rows, q2spot.Row{Kind: "info", Title: t})
	}
	d := float64(a.delay()) / 1000
	rows = append(rows, q2spot.Row{Kind: "action", URI: "delay", Title: fmt.Sprintf("Delay: %g s", d),
		Subtitle: "How far behind the Q2 the speaker plays; more rides out weak Wi-Fi"})
	return q2spot.Reply{Message: "refresh", Rows: rows}
}

// maker is a speaker's maker, from its model: "Sonos", "Apple"…; "AirPlay" when unknown.
func maker(model string) string {
	m := strings.ToLower(model)
	switch {
	case strings.Contains(m, "sonos"):
		return "Sonos"
	case strings.HasPrefix(m, "audioaccessory"):
		return "HomePod"
	case strings.HasPrefix(m, "appletv"):
		return "Apple TV"
	case strings.HasPrefix(m, "airport"):
		return "AirPort Express"
	case strings.HasPrefix(m, "mac") || strings.HasPrefix(m, "imac"):
		return "Mac"
	}
	return "AirPlay"
}
