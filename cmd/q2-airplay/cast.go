package main

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/group-a-study/q2-airplay/internal/q2"
)

// The engine plays what the Q2 plays on one speaker, a fixed delay (delayMS) behind it: frame f of the
// tap, put into the ring at tap time t, is heard at t + delay, so the speaker does what the Q2 did, a
// moment later. A pause is a pause there too: the speaker plays what it was sent, then the stream is
// flushed and waits (nothing more to send, so the Wi-Fi idles); Play starts it again from the next
// frame. A seek or a skip (the filter's jumps) flushes the speaker's queue and starts at the new place,
// a song running into the next doesn't. The Q2's own output is muted from the first start until the
// speaker is let go (or this app stops beating: the payload lets go by itself).

const (
	tickPlaying   = 100 * time.Millisecond // sending: the tap's ring holds 5.9 s, the delay covers it
	tickWaiting   = 250 * time.Millisecond // connecting, paused or parked: within a quarter second of Play
	tickIdle      = time.Second            // no speaker chosen
	batchMS       = 200                    // audio is sent in bursts this long: the radio sleeps between them
	gapMS         = 150                    // tap frames this much later than their place on the speaker's timeline: a gap (pause, stall), kept as silence
	restartMS     = 5000                   // a gap this long: flushed and started again instead
	parkAfterMS   = 100                    // paused this long past the last frame heard: flushed and parked
	minLeadMS     = 300                    // a start at least this far ahead
	positionEvery = 200 * time.Millisecond
	readyWait     = 6 * time.Second // the speaker's clock not ready by then: started anyway
	maxQueued     = 2 * time.Second // sent but not taken by cliairplay this long: it's stuck
)

type phase int

const (
	phConnecting phase = iota // cliairplay connecting, the speaker's clock settling
	phReady                   // connected: the first start waits for something to play
	phStarting                // START sent, its ack awaited
	phPlaying                 // frames flowing
	phFlushing                // FLUSH sent (a pause, or a jump), its ack awaited
	phParked                  // flushed: the next start waits for play
)

// run is a span of fed frames: seq's frames from aud (CLOCK_MONOTONIC ms) on.
type run struct {
	aud float64
	seq uint32
	n   int
}

// obs is a sight of the filter's head: frame seq written by mono time stamp, at song place pts (ms)
// of filter instance id.
type obs struct {
	seq   uint32
	stamp int64
	pts   int32
	id    uint32
}

type session struct {
	sp         speaker
	snd        *sender
	compat     bool // the AirPlay 1 flow, after AirPlay 2 failed
	phase      phase
	connected  time.Time
	clockReady bool
	fed        uint32  // the next frame to send
	nextAud    float64 // CLOCK_MONOTONIC ms the next frame sent is heard at
	pending    []byte
	lastSend   time.Time
	started    bool // a start was made: the Q2's own output is muted
	jumps      uint32
	jumpTo     uint32 // the frame to start from once a jump's flush is acked
	jumping    bool
	volume     int32
	track      uint32
	metaAt     time.Time // when the queued song's tags go out
	runs       []run
	lastPos    time.Time
	err        string
}

type engine struct {
	t    *tap
	opt  engineOptions
	poke chan struct{} // a choice: look at once

	mu            sync.Mutex
	want          *speaker // the chosen speaker; nil: this Q2
	s             *session
	status        string // the page's status line
	obs           []obs
	retryAt       time.Time
	retries       int
	asked         *levelAsk      // the chosen speaker\'s volume being read
	levels        map[string]int // each speaker\'s last volume
	levelsChanged bool
}

type engineOptions struct {
	bin, cmdpipe, dacp, remote, artFile string
	track                               string         // CAST_TRACK
	levels                              map[string]int // each speaker's last volume, by name
	delayMS                             func() int
}

func newEngine(t *tap, o engineOptions) *engine {
	levels := map[string]int{}
	for k, v := range o.levels {
		levels[k] = v
	}
	return &engine{t: t, opt: o, poke: make(chan struct{}, 1), levels: levels}
}

// levelAsk is a speaker's own volume being read (speakerLevel), once a choice.
type levelAsk struct {
	key          string
	done         chan struct{}
	level        int
	known        bool
	waitingSince time.Time
}

// takeLevels is the speakers' volumes when one changed since the last call, for the settings.
func (e *engine) takeLevels() (map[string]int, bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.levelsChanged {
		return nil, false
	}
	e.levelsChanged = false
	m := make(map[string]int, len(e.levels))
	for k, v := range e.levels {
		m[k] = v
	}
	return m, true
}

// choose picks a speaker (nil: this Q2 again).
func (e *engine) choose(sp *speaker) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.want = sp
	e.retries = 0
	e.retryAt = time.Time{}
	if e.s != nil && (sp == nil || sp.key() != e.s.sp.key()) {
		e.end("")
	}
	e.asked = nil
	if sp != nil { // the speaker's own volume, asked aside so nothing waits on it
		ask := &levelAsk{key: sp.key(), done: make(chan struct{})}
		e.asked = ask
		go func(sp speaker) {
			ask.level, ask.known = speakerLevel(sp.IP, sp.Port, e.opt.dacp, e.opt.remote)
			close(ask.done)
		}(*sp)
	}
	if sp == nil {
		e.status = ""
	}
	select {
	case e.poke <- struct{}{}:
	default:
	}
}

// state is what the page shows: the chosen speaker's key, and a line about it.
func (e *engine) state() (key, line string, playing bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.want == nil {
		return "", e.status, false
	}
	return e.want.key(), e.status, e.s != nil && e.s.started
}

func (e *engine) active() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.want != nil
}

func (e *engine) run(stop <-chan struct{}) {
	timer := time.NewTimer(tickIdle)
	defer timer.Stop()
	for {
		select {
		case <-stop:
			e.mu.Lock()
			e.want = nil
			e.end("")
			e.mu.Unlock()
			return
		case <-e.poke:
			if !timer.Stop() {
				<-timer.C
			}
			timer.Reset(0)
		case <-timer.C:
			e.mu.Lock()
			e.step(time.Now())
			timer.Reset(e.every())
			e.mu.Unlock()
		}
	}
}

// every is how soon the engine looks again: often while it sends, seldom while nothing moves, so a
// pause costs the battery little.
func (e *engine) every() time.Duration {
	switch {
	case e.want == nil:
		return tickIdle
	case e.s != nil && (e.s.phase == phPlaying || (e.s.phase == phFlushing && e.s.jumping)):
		return tickPlaying // sending, or a pause playing out
	}
	return tickWaiting
}

// end lets the speaker go and the Q2 play aloud again.
func (e *engine) end(why string) {
	if e.s != nil {
		e.s.snd.stop()
		e.s = nil
	}
	e.t.stop()
	if why != "" {
		e.status = why
	}
}

// The speaker's timeline is kept in CLOCK_MONOTONIC ms, so a jump of the wall clock (set from the
// network once Wi-Fi is back, say) moves nothing; only START's instant is wall-clock, as cliairplay
// takes it.
func monoMS(ns int64) float64 { return float64(ns) / 1e6 }

// unixAt is the wall-clock ms of the monotonic ms at.
func unixAt(at float64, now time.Time, monoNow int64) int64 {
	return int64(at + float64(now.UnixNano())/1e6 - monoMS(monoNow))
}

// see keeps the filter's head when it moved, for tap times and song places (10 s of them).
func (e *engine) see(h head) {
	if len(e.obs) > 0 && e.obs[len(e.obs)-1].seq == h.seq {
		return
	}
	e.obs = append(e.obs, obs{h.seq, h.stamp, h.pts, h.id})
	if len(e.obs) > 300 {
		e.obs = e.obs[len(e.obs)-200:]
	}
}

// tapTime is the mono time frame seq was (or will be) written: between the sights around it, else on
// from the nearest at tapRate.
func (e *engine) tapTime(seq uint32) int64 {
	if len(e.obs) == 0 {
		return q2.MonoNS()
	}
	for i := len(e.obs) - 1; i >= 0; i-- {
		o := e.obs[i]
		if int32(seq-o.seq) <= 0 && i > 0 {
			p := e.obs[i-1]
			if int32(seq-p.seq) >= 0 {
				// written in o's block: its frames' times run up to o.stamp at tapRate
				return o.stamp - int64(o.seq-seq)*1e9/tapRate
			}
			continue
		}
		return o.stamp + int64(int32(seq-o.seq))*1e9/tapRate
	}
	o := e.obs[0]
	return o.stamp - int64(int32(o.seq-seq))*1e9/tapRate
}

// place is frame seq's song (filter instance) and its place in it, ms.
func (e *engine) place(seq uint32) (uint32, int32) {
	for i := len(e.obs) - 1; i >= 0; i-- {
		o := e.obs[i]
		if int32(seq-o.seq) <= 0 && (i == 0 || int32(seq-e.obs[i-1].seq) > 0) {
			return o.id, o.pts - int32(int64(o.seq-seq)*1000/tapRate)
		}
	}
	if n := len(e.obs); n > 0 {
		o := e.obs[n-1]
		return o.id, o.pts + int32(int64(int32(seq-o.seq))*1000/tapRate)
	}
	return 0, 0
}

func (e *engine) step(now time.Time) {
	t := e.t
	monoNow := q2.MonoNS()
	h := t.head()
	e.see(h)
	if e.want == nil {
		return
	}
	s := e.s
	if s == nil {
		waiting := false
		if a := e.asked; a != nil {
			select {
			case <-a.done:
			default:
				if a.waitingSince.IsZero() {
					a.waitingSince = now
				}
				waiting = now.Sub(a.waitingSince) < infoTimeout+500*time.Millisecond
			}
		}
		if now.Before(e.retryAt) || waiting {
			t.beat(monoNow, false, castConnecting)
			return
		}
		if err := e.begin(now, monoNow, h, e.retries > 0 && e.retries%2 == 1); err != nil {
			e.status = "Can't start AirPlay: " + err.Error()
			e.want = nil
			t.stop()
			return
		}
		s = e.s
	}
	state := castConnecting
	if s.started {
		state = castOn
	}
	t.beat(monoNow, s.started, state)
	e.events(now, monoNow, h)
	if e.s == nil { // it ended
		return
	}
	switch s.phase {
	case phConnecting:
		if !s.connected.IsZero() && (s.clockReady || now.Sub(s.connected) > readyWait) {
			s.phase = phReady
			s.fed = h.seq
		}
	case phReady: // nothing sent yet: start from what plays now
		s.jumps = h.jumps
		if t.status() == playPlaying && monoNow-h.stamp < int64(500*time.Millisecond) {
			e.start(now, monoNow, h.seq)
		}
	case phParked:
		if h.jumps != s.jumps { // a seek or skip while parked: start from it
			s.jumps = h.jumps
			s.fed = h.jumpSeq
		}
		if t.status() == playPlaying && h.seq != s.fed {
			e.start(now, monoNow, s.fed)
		}
	case phPlaying:
		if h.jumps != s.jumps {
			s.jumps = h.jumps
			if int32(h.jumpSeq-s.fed) > -tapRate*30 { // the jump is among what we have
				e.flush(true, h.jumpSeq)
				break
			}
		}
		e.feed(now, monoNow, h)
		if e.s == nil || s.phase != phPlaying { // it ended, or started again
			break
		}
		if t.status() != playPlaying && h.seq == s.fed {
			e.sendPending(now, true)
			if e.s != nil && monoMS(monoNow) > s.nextAud+parkAfterMS {
				e.flush(false, s.fed)
			}
		}
	}
	if e.s == nil { // cliairplay stopped taking audio: lost
		return
	}
	if h.dsd && t.status() == playPlaying && s.phase != phConnecting {
		e.status = "DSD plays on the Q2 only"
	} else if s.started && s.err == "" {
		e.status = "Playing on " + s.sp.Name
	}
	e.extras(now, monoNow)
}

// begin starts cliairplay for the chosen speaker; compat: the AirPlay 1 flow.
func (e *engine) begin(now time.Time, monoNow int64, h head, compat bool) error {
	sp := *e.want
	proto := "auto"
	if compat && sp.AirPlay2 {
		proto = "airplay2-compat"
	}
	// The volume: the speaker's own when it says (and cliairplay leaves it), else the one it had last
	// from the Q2, set as it connects. demo shows and turns from it; the earphones keep theirs.
	vol, known := 35, false
	if a := e.asked; a != nil && a.key == sp.key() {
		select {
		case <-a.done:
			vol, known = a.level, a.known
		default:
		}
	}
	if v, ok := e.levels[sp.Name]; ok && !known {
		vol = v
	}
	send := clampVolume(vol)
	if known {
		send = 0 // as it is
	}
	snd, err := startSender(senderOptions{bin: e.opt.bin, cmdpipe: e.opt.cmdpipe, dacp: e.opt.dacp,
		remote: e.opt.remote, name: "Shanling Q2", protocol: proto, volume: send,
		latencyMS: e.opt.delayMS() + 500, sp: sp})
	if err != nil {
		return err
	}
	e.s = &session{sp: sp, snd: snd, compat: compat, volume: int32(vol), jumps: h.jumps,
		track: e.t.track()} // start sends the tags
	e.t.setLevel(clampVolume(vol))
	e.t.setSpeaker(sp.Name)
	e.status = "Connecting to " + sp.Name + "…"
	return nil
}

func clampVolume(v int) int {
	if v < 0 {
		return 0
	}
	if v > 100 {
		return 100
	}
	return v
}

// events takes cliairplay's status lines.
func (e *engine) events(now time.Time, monoNow int64, h head) {
	s := e.s
	for {
		var ev event
		select {
		case ev = <-s.snd.events:
		default:
			return
		}
		switch ev.kind {
		case "connected":
			s.connected = now
			e.status = "Playing on " + s.sp.Name
			e.retries = 0
		case "clock_ready":
			st := ev.kv["state"]
			s.clockReady = s.clockReady || st == "ready" || ev.kv["mode"] == "ntp"
		case "started":
			// The speaker may start later than asked: everything sent moves with it.
			if d := float64(ev.int64("at_unix_ms") - ev.int64("requested_unix_ms")); d > 0 {
				s.nextAud += d
				for i := range s.runs {
					s.runs[i].aud += d
				}
			}
		case "anchor_corrected":
			if d := float64(ev.int64("at_unix_ms") - ev.int64("from_unix_ms")); d > 0 {
				s.nextAud += d
			}
		case "REANCHOR":
			ms := float64(ev.int64("shifted_frames")) * 1000 / tapRate
			s.nextAud += ms
			for i := range s.runs {
				if s.runs[i].aud+float64(s.runs[i].n)*1000/tapRate > monoMS(monoNow) {
					s.runs[i].aud += ms
				}
			}
		case "flushed":
			if s.phase == phFlushing {
				s.phase = phParked
				if s.jumping {
					s.jumping = false
					s.fed = s.jumpTo
					e.start(now, monoNow, s.fed)
				}
			}
		case "error", "fatal":
			if d := ev.kv["detail"]; d != "" {
				s.err = d
			}
		case "exit":
			e.lost(now, h)
			return
		}
	}
}

// lost: cliairplay ended without being asked. AirPlay 2 failing before it connected tries AirPlay 1
// next; then again after a pause, longer each time.
func (e *engine) lost(now time.Time, h head) {
	s := e.s
	e.s = nil
	s.snd.stop() // its writer let go; a stuck one killed
	e.t.stop()
	why := s.err
	if why == "" {
		why = "the connection ended"
	}
	e.retries++
	if s.connected.IsZero() && s.sp.AirPlay2 && !s.compat {
		e.retryAt = now // straight to AirPlay 1
		e.status = "Connecting to " + s.sp.Name + "…"
		return
	}
	wait := time.Duration(e.retries) * 3 * time.Second
	if wait > 30*time.Second {
		wait = 30 * time.Second
	}
	e.retryAt = now.Add(wait)
	e.status = s.sp.Name + ": " + why
}

// start schedules frame from to be heard delay after it was put in the ring.
func (e *engine) start(now time.Time, monoNow int64, from uint32) {
	s := e.s
	at := monoMS(e.tapTime(from)) + float64(e.opt.delayMS())
	if earliest := monoMS(monoNow) + minLeadMS; at < earliest {
		at = earliest
	}
	if !s.started {
		e.metadata(now) // Sonos plays nothing before it has the tags
	}
	s.fed = from
	s.nextAud = at
	s.runs = s.runs[:0]
	s.pending = s.pending[:0]
	s.snd.command(fmt.Sprintf("START_UNIX_MS=%d", unixAt(at, now, monoNow)), "ACTION=START")
	s.phase = phPlaying
	s.started = true
}

// feed sends the frames the filter wrote since the last, after a silence where they came late.
func (e *engine) feed(now time.Time, monoNow int64, h head) {
	s := e.s
	n := int(int32(h.seq - s.fed))
	if n <= 0 {
		return
	}
	if n > tapRing-tapRate { // fell a ring behind: start again from what plays now
		e.flush(true, h.seq)
		return
	}
	want := monoMS(e.tapTime(s.fed)) + float64(e.opt.delayMS())
	gap := want - s.nextAud
	if gap > restartMS { // too long to bridge with silence: start again from here
		e.flush(true, s.fed)
		return
	}
	if gap > gapMS { // a pause or a stall on the Q2: the same silence here
		k := int(gap * tapRate / 1000)
		s.pending = append(s.pending, make([]byte, 4*k)...)
		s.nextAud += float64(k) * 1000 / tapRate
	}
	s.runs = append(s.runs, run{s.nextAud, s.fed, n})
	if len(s.runs) > 400 {
		s.runs = s.runs[len(s.runs)-200:]
	}
	s.pending = e.t.frames(s.pending, s.fed, n)
	s.fed += uint32(n)
	s.nextAud += float64(n) * 1000 / tapRate
	e.sendPending(now, false) // may end the session
}

// sendPending hands the batch to cliairplay once it holds batchMS (or now, when all).
func (e *engine) sendPending(now time.Time, all bool) {
	s := e.s
	if len(s.pending) == 0 || (!all && len(s.pending) < 4*tapRate*batchMS/1000 && now.Sub(s.lastSend) < batchMS*time.Millisecond) {
		return
	}
	if !s.snd.send(s.pending) {
		e.lost(now, head{}) // cliairplay stopped reading
		return
	}
	s.pending = nil
	s.lastSend = now
}

// flush ends what the speaker plays: a pause once it is all heard, or a jump at once (then starting
// from the frame to). cliairplay drops what it still holds of stdin, so the writer is let finish
// first.
func (e *engine) flush(jump bool, to uint32) {
	s := e.s
	s.pending = s.pending[:0]
	s.snd.drained(time.Second)
	s.snd.command("ACTION=FLUSH")
	s.phase = phFlushing
	s.jumping = jump
	s.jumpTo = to
	s.fed = to
}

// extras: the volume, the song's tags once its first frame is heard, and the speaker's place for
// the lyrics.
func (e *engine) extras(now time.Time, monoNow int64) {
	s := e.s
	if v := e.t.volume(); v >= 0 && v != s.volume { // demo's -1: not the speaker's yet
		s.volume = v
		s.snd.command("VOLUME=" + strconv.Itoa(clampVolume(int(v))))
		e.levels[s.sp.Name] = clampVolume(int(v))
		e.levelsChanged = true
	}
	if tr := e.t.track(); tr != s.track {
		s.track = tr
		s.metaAt = now.Add(time.Duration(e.opt.delayMS()) * time.Millisecond)
	}
	if !s.metaAt.IsZero() && !now.Before(s.metaAt) && s.started {
		e.metadata(now)
	}
	if now.Sub(s.lastPos) >= positionEvery && s.started {
		s.lastPos = now
		e.position(now, monoNow)
	}
}

// metadata sends the playing song's tags and art (CAST_TRACK, demo's).
func (e *engine) metadata(now time.Time) {
	s := e.s
	s.metaAt = time.Time{}
	tags := readTrack(e.opt.track)
	lines := []string{
		"TITLE=" + oneLine(tags["title"]), "ARTIST=" + oneLine(tags["artist"]),
		"ALBUM=" + oneLine(tags["album"]), "DURATION=" + tags["length"],
		"ITEMID=" + oneLine(tags["path"]),
	}
	art := ""
	if p := tags["path"]; strings.HasPrefix(p, "/") && e.opt.artFile != "" {
		art = findArt(p, e.opt.artFile)
	}
	lines = append(lines, "ARTWORKFILE="+art, "ACTION=SENDMETA")
	s.snd.command(lines...)
}

// position tells demo where in the song the speaker is.
func (e *engine) position(now time.Time, monoNow int64) {
	s := e.s
	at := monoMS(monoNow)
	for i := len(s.runs) - 1; i >= 0; i-- {
		r := s.runs[i]
		if at < r.aud {
			continue
		}
		k := int((at - r.aud) * tapRate / 1000)
		if k >= r.n { // past the last frame sent: it stands there
			id, pts := e.place(r.seq + uint32(r.n))
			e.t.position(pts, id, false, monoNow)
			return
		}
		id, pts := e.place(r.seq + uint32(k))
		e.t.position(pts, id, s.phase == phPlaying, monoNow)
		return
	}
}

func oneLine(s string) string { return strings.NewReplacer("\n", " ", "\r", " ").Replace(s) }

// readTrack reads CAST_TRACK's key=value lines.
func readTrack(path string) map[string]string {
	m := map[string]string{}
	b, err := os.ReadFile(path)
	if err != nil {
		return m
	}
	for _, line := range strings.Split(string(b), "\n") {
		if i := strings.IndexByte(line, '='); i > 0 {
			m[line[:i]] = line[i+1:]
		}
	}
	return m
}
