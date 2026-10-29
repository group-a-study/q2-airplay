package main

import (
	"bufio"
	"encoding/binary"
	"encoding/hex"
	"fmt"

	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/group-a-study/q2-airplay/internal/q2"
	"github.com/group-a-study/q2-airplay/internal/q2spot"
)

// TestMain runs as a stand-in cliairplay when FAKE_CLIAIRPLAY names a directory: it records its
// commands and its stdin there and answers as the real one does (DESIGN.md's [STATUS] lines).
func TestMain(m *testing.M) {
	if dir := os.Getenv("FAKE_CLIAIRPLAY"); dir != "" {
		os.Exit(fakeSender(dir, os.Args[1:]))
	}
	os.Exit(m.Run())
}

// fakeSender also appends its arguments to starts, a line each run. A file fail-PROTOCOL there makes
// a run with that --protocol fail before it connects; TEST=IDLE on the command pipe ends it as
// parked 2 min does, TEST=EXIT as a lost connection does.
func fakeSender(dir string, args []string) int {
	pipe, proto := "", ""
	for i, a := range args {
		switch a {
		case "--cmdpipe":
			pipe = args[i+1]
		case "--protocol":
			proto = args[i+1]
		}
	}
	audio, _ := os.Create(filepath.Join(dir, "audio"))
	var mu sync.Mutex
	marks, _ := os.Create(filepath.Join(dir, "commands"))
	os.WriteFile(filepath.Join(dir, "args"), []byte(strings.Join(args, "\n")), 0o644)
	if f, err := os.OpenFile(filepath.Join(dir, "starts"), os.O_WRONLY|os.O_APPEND|os.O_CREATE, 0o644); err == nil {
		fmt.Fprintln(f, strings.Join(args, " "))
		f.Close()
	}
	if _, err := os.Stat(filepath.Join(dir, "fail-"+proto)); err == nil {
		fmt.Fprintln(os.Stderr, "[STATUS] error code=connect_failed http=0 detail=\"no answer\"")
		return 1
	}
	got := 0
	go func() {
		buf := make([]byte, 65536)
		for {
			n, err := os.Stdin.Read(buf)
			mu.Lock()
			audio.Write(buf[:n])
			got += n
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	fmt.Println("[STATUS] route protocol=airplay2 flow=native timing=ptp buffered=0")
	fmt.Fprintln(os.Stderr, "[12:00:00.000] some_log:1 human output")
	fmt.Fprintln(os.Stderr, "[STATUS] connected")
	fmt.Fprintln(os.Stderr, "[STATUS] clock_ready mode=ptp state=ready streak_ms=2400 exchanges=12 ready_in_ms=0 ready_at_unix_ms=0")
	f, err := os.Open(pipe)
	if err != nil {
		return 0
	}
	sc := bufio.NewScanner(f)
	start := ""
	for sc.Scan() {
		line := sc.Text()
		mu.Lock()
		fmt.Fprintf(marks, "%d %s\n", got, line) // the audio bytes taken before each command
		mu.Unlock()
		switch {
		case strings.HasPrefix(line, "START_UNIX_MS="):
			start = line[len("START_UNIX_MS="):]
		case line == "ACTION=START":
			fmt.Fprintf(os.Stderr, "[STATUS] started requested_unix_ms=%s at_unix_ms=%s\n", start, start)
		case line == "ACTION=FLUSH":
			fmt.Fprintln(os.Stderr, "[STATUS] flushed")
		case line == "ACTION=STOP":
			fmt.Fprintln(os.Stderr, "[STATUS] stopped")
			return 0
		case line == "TEST=IDLE":
			fmt.Fprintln(os.Stderr, "[STATUS] idle_timeout")
			return 0
		case line == "TEST=EXIT":
			return 0
		}
	}
	return 0
}

func TestParseStatus(t *testing.T) {
	e, ok := parseStatus(`[STATUS] error code=auth_required http=0 detail="Password required but not supplied"`)
	if !ok || e.kind != "error" || e.kv["code"] != "auth_required" || e.kv["detail"] != "Password required but not supplied" {
		t.Fatalf("%v", e)
	}
	e, _ = parseStatus("[STATUS] started requested_unix_ms=1700000000000 at_unix_ms=1700000000250")
	if e.kind != "started" || e.int64("at_unix_ms")-e.int64("requested_unix_ms") != 250 {
		t.Fatalf("%v", e)
	}
	e, _ = parseStatus("[STATUS] flushed head_unix_ms=5")
	if e.kind != "flushed" || e.kv["head_unix_ms"] != "5" {
		t.Fatalf("%v", e)
	}
	if _, ok := parseStatus("[12:00:00.000] ap2cl_connect:3469 [AP2] Connecting"); ok {
		t.Fatal("a log line taken as status")
	}
	if e, ok := parseStatus("[ERROR] Cannot connect to AirPlay 2 device"); !ok || e.kind != "fatal" {
		t.Fatalf("%v", e)
	}
}

// response builds an mDNS answer: a PTR to instance, its SRV and TXT, and the host's A, names after
// the first compressed.
func response(instance, typ, host string, port int, txt []string, ip net.IP) []byte {
	b := make([]byte, 12)
	binary.BigEndian.PutUint16(b[2:], 0x8400)
	binary.BigEndian.PutUint16(b[6:], 4)
	rr := func(name []byte, t uint16, data []byte) {
		b = append(b, name...)
		b = binary.BigEndian.AppendUint16(b, t)
		b = binary.BigEndian.AppendUint16(b, 1)
		b = binary.BigEndian.AppendUint32(b, 120)
		b = binary.BigEndian.AppendUint16(b, uint16(len(data)))
		b = append(b, data...)
	}
	typeAt := len(b)
	rr(encodeName(nil, typ), dnsPTR, nil)
	// patch the PTR's data in: the instance's label, then a pointer to the type
	ptr := append([]byte{byte(len(instance))}, instance...)
	ptr = append(ptr, 0xc0|byte(typeAt>>8), byte(typeAt))
	b = b[:len(b)-2]
	b = binary.BigEndian.AppendUint16(b, uint16(len(ptr)))
	instAt := len(b)
	b = append(b, ptr...)
	inst := []byte{0xc0 | byte(instAt>>8), byte(instAt)}
	srv := binary.BigEndian.AppendUint16([]byte{0, 0, 0, 0}, uint16(port))
	srv = encodeName(srv, host)
	rr(inst, dnsSRV, srv)
	var t []byte
	for _, s := range txt {
		t = append(append(t, byte(len(s))), s...)
	}
	rr(inst, dnsTXT, t)
	rr(encodeName(nil, host), dnsA, ip.To4())
	return b
}

func TestBrowser(t *testing.T) {
	b := &browser{services: map[string]*service{}, hosts: map[string]net.IP{}, self: map[string]bool{"10.0.0.9": true}}
	now := time.Now()
	for _, m := range [][]byte{
		response("Kitchen", airplayType, "Sonos-1.local.", 7000, []string{"features=0x445F8A00,0x1C340", "model=Era 100", "manufacturer=Sonos"}, net.IPv4(10, 0, 0, 5)),
		response("5465E1234567@Kitchen", raopType, "Sonos-1.local.", 7000, []string{"et=0,4", "cn=0,1", "am=Era 100"}, net.IPv4(10, 0, 0, 5)),
		response("Mr. Speaker", raopType, "old.local.", 5000, []string{"et=0,1", "cn=0,1", "am=AirPort10,115"}, net.IPv4(10, 0, 0, 6)),
		response("Q2", raopType, "q2.local.", 5000, []string{"et=0"}, net.IPv4(10, 0, 0, 9)), // this Q2's own
	} {
		rrs, err := parseMessage(m)
		if err != nil {
			t.Fatal(err)
		}
		b.take(rrs, now)
	}
	sp := b.speakers(now)
	if len(sp) != 2 {
		t.Fatalf("%+v", sp)
	}
	k, m := sp[0], sp[1]
	if k.Name != "Kitchen" || k.IP != "10.0.0.5" || k.Port != 7000 || !k.AirPlay2 || k.RaopTXT["et"] != "0,4" ||
		!strings.Contains(k.TXT, "features=0x445F8A00,0x1C340") || maker(k.Model+" "+k.TXT) != "Sonos" {
		t.Fatalf("%+v", k)
	}
	if m.Name != "Mr. Speaker" || m.AirPlay2 || m.Port != 5000 || maker(m.Model) != "AirPort Express" {
		t.Fatalf("%+v", m)
	}
	// a name with a dot goes back out as one label
	name, _, err := readName(encodeName(nil, `Mr\. Speaker._raop._tcp.local.`), 0)
	if err != nil || name != `Mr\. Speaker._raop._tcp.local.` {
		t.Fatalf("%q %v", name, err)
	}
	if len(b.speakers(now.Add(speakerStale+time.Second))) != 0 {
		t.Fatal("stale speakers kept")
	}
}

// filter stands in for hciplayer's PEQ filter: frames at tapRate in 100 ms blocks, each frame's left
// sample its own number (mod 65536), so the stream's continuity shows.
type filter struct {
	t       *tap
	mu      sync.Mutex
	playing bool
	stalled bool // playing, but writing nothing: a starved player
	pts     int32
	id      uint32
	stop    chan struct{}
}

func (f *filter) run() {
	tick := time.NewTicker(100 * time.Millisecond)
	defer tick.Stop()
	for {
		select {
		case <-f.stop:
			return
		case <-tick.C:
		}
		f.mu.Lock()
		if f.playing && !f.stalled {
			f.block(tapRate / 10)
		}
		f.mu.Unlock()
	}
}

func (f *filter) block(n int) {
	seq := f.t.u32(offSeq)
	for i := 0; i < n; i++ {
		at := offRing + 4*int((seq+uint32(i))&(tapRing-1))
		binary.LittleEndian.PutUint16(f.t.mem[at:], uint16(seq+uint32(i)))
		binary.LittleEndian.PutUint16(f.t.mem[at+2:], uint16(seq+uint32(i)))
	}
	f.pts += int32(n * 1000 / tapRate)
	f.t.set(offID, f.id)
	f.t.set(offPTS, uint32(f.pts))
	now := q2.MonoNS()
	f.t.set(offStamp, uint32(now))
	f.t.set(offStamp+4, uint32(uint64(now)>>32))
	f.t.set(offSeq, seq+uint32(n))
}

func (f *filter) set(playing bool) {
	f.mu.Lock()
	f.playing = playing
	status := playPaused
	if playing {
		status = playPlaying
	}
	f.t.set(offStatus, uint32(status))
	f.mu.Unlock()
}

// jump is a seek: the frames from now on follow no earlier audio.
func (f *filter) jump() uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	seq := f.t.u32(offSeq)
	f.t.set(offJumpSeq, seq)
	f.t.set(offJumps, f.t.u32(offJumps)+1)
	f.pts = 60000
	return seq
}

func waitFor(t *testing.T, what string, d time.Duration, ok func() bool) {
	t.Helper()
	end := time.Now().Add(d)
	for !ok() {
		if time.Now().After(end) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func commands(dir string) []string {
	b, _ := os.ReadFile(filepath.Join(dir, "commands"))
	return strings.Split(strings.TrimSpace(string(b)), "\n")
}

func has(dir, cmd string) int {
	n := 0
	for _, c := range commands(dir) {
		if strings.HasSuffix(c, " "+cmd) {
			n++
		}
	}
	return n
}

// infoSpeaker answers GET /info as an AirPlay 2 speaker does, with initialVolume db, until the test
// ends; its port.
func infoSpeaker(t *testing.T, plist []byte) int {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.Close() })
	go func() {
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			go func() {
				defer c.Close()
				r := bufio.NewReader(c)
				for {
					line, err := r.ReadString('\n')
					if err != nil || line == "\r\n" {
						break
					}
				}
				fmt.Fprintf(c, "RTSP/1.0 200 OK\r\nContent-Type: application/x-apple-binary-plist\r\nCSeq: 1\r\nContent-Length: %d\r\n\r\n", len(plist))
				c.Write(plist)
			}()
		}
	}()
	return l.Addr().(*net.TCPAddr).Port
}

func mustHex(s string) []byte {
	b, err := hex.DecodeString(s)
	if err != nil {
		panic(err)
	}
	return b
}

// Binary plists as Python's plistlib writes them: /info-like dictionaries.
var (
	plistReal = mustHex("62706c6973743030d60102030405060708090a0b0c5864657669636549445866656174757265735d696e697469616c566f6c756d65556d6f64656c546e616d655b737461747573466c6167735541413a4242120012d68723c0280000000000005745726120313030574b69746368656e100408151e27353b404c52576068700000000000000101000000000000000d00000000000000000000000000000072") // initialVolume -12.0
	plistInt  = mustHex("62706c6973743030d2010203045d696e697469616c566f6c756d65556d6f64656c10005158080d1b21230000000000000101000000000000000500000000000000000000000000000025")                                                                                                                                                                           // initialVolume 0
	plistMute = mustHex("62706c6973743030d101025d696e697469616c566f6c756d6523c062000000000000080b190000000000000101000000000000000300000000000000000000000000000022")                                                                                                                                                                                     // initialVolume -144.0
	plistNone = mustHex("62706c6973743030d301020304050b556d6f64656c566e65737465645a747874416972506c617955536f6e6f73d106075161a308090a100110021003460568656c6c6f080f151c272d303236383a3c0000000000000101000000000000000c00000000000000000000000000000043")                                                                                                 // no initialVolume
)

func TestLevel(t *testing.T) {
	for _, c := range []struct {
		b    []byte
		want float64
		ok   bool
	}{{plistReal, -12, true}, {plistInt, 0, true}, {plistMute, -144, true}, {plistNone, 0, false}, {[]byte("bplist00 junk"), 0, false}} {
		if v, ok := plistNumber(c.b, "initialVolume"); ok != c.ok || v != c.want {
			t.Fatalf("%v %v, want %v %v", v, ok, c.want, c.ok)
		}
	}
	// level inverts libraop's raopcl_float_volume (-30 + 30 * v / 100) for every v, 0 muted
	for v := 1; v <= 100; v++ {
		if got := level(-30 + 30*float64(v)/100); got != v {
			t.Fatalf("level of %d's dB is %d", v, got)
		}
	}
	if level(-144) != 0 || level(-30) != 0 || level(0) != 100 || level(-12) != 60 {
		t.Fatal("level's ends")
	}
	if v, ok := speakerLevel("127.0.0.1", infoSpeaker(t, plistReal), "0011223344556677", "1"); !ok || v != 60 {
		t.Fatalf("speakerLevel %d %v", v, ok)
	}
	if _, ok := speakerLevel("127.0.0.1", infoSpeaker(t, plistNone), "0011223344556677", "1"); ok {
		t.Fatal("a level from a speaker that didn't say")
	}
}

func TestEngine(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	os.Mkdir(fake, 0o755)
	t.Setenv("FAKE_CLIAIRPLAY", fake)
	tp, err := openTap(filepath.Join(dir, "q2cast"))
	if err != nil {
		t.Fatal(err)
	}
	if again, err := openTap(filepath.Join(dir, "q2cast")); err != nil || again == nil {
		t.Fatal("an existing tap isn't mapped again", err)
	}
	os.WriteFile(filepath.Join(dir, "track"), []byte("path=/mnt/mmc/a.flac\ntitle=Money\nartist=Pink Floyd\nalbum=DSOTM\nlength=382\n"), 0o644)
	tp.set(offVolume, 0xffffffff) // demo's -1 until the speaker plays: nothing sent

	f := &filter{t: tp, id: 1, stop: make(chan struct{})}
	defer close(f.stop)
	f.set(true)
	go f.run()
	const delay = 400
	e := newEngine(tp, engineOptions{bin: os.Args[0], cmdpipe: filepath.Join(dir, "cmd"), dacp: "0011223344556677",
		remote: "1", track: filepath.Join(dir, "track"), levels: map[string]int{"Kitchen": 42},
		delayMS: func() int { return delay }})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { e.run(stop); close(done) }()
	// a speaker at 60 (-12 dB): the Q2 takes its volume, and cliairplay leaves it
	sp := speaker{Name: "Kitchen", IP: "127.0.0.1", Port: infoSpeaker(t, plistReal), TXT: "features=0x445F8A00,0x1C340", AirPlay2: true}
	e.choose(&sp)

	// Connected, then started from what plays: the Q2 muted, demo told it plays.
	waitFor(t, "START", 3*time.Second, func() bool { return has(fake, "ACTION=START") == 1 })
	args, _ := os.ReadFile(filepath.Join(fake, "args"))
	for _, want := range []string{"--protocol\nauto", "--volume\n0", "--txt\nfeatures=0x445F8A00,0x1C340", "--latency\n900",
		"--debug\n0", "--clock-offset-ns\n", "127.0.0.1"} {
		if !strings.Contains(string(args), want) {
			t.Fatalf("args %q lack %q", args, want)
		}
	}
	for _, want := range []string{"TITLE=Money", "ARTIST=Pink Floyd", "ALBUM=DSOTM", "DURATION=382", "ACTION=SENDMETA"} {
		if has(fake, want) == 0 {
			t.Fatalf("no %s before START: %v", want, commands(fake))
		}
	}
	var startAt int64
	for _, c := range commands(fake) {
		if _, v, ok := strings.Cut(c, " START_UNIX_MS="); ok {
			startAt, _ = strconv.ParseInt(v, 10, 64)
		}
	}
	if d := startAt - time.Now().UnixMilli(); d < 0 || d > delay+150 {
		t.Fatalf("starts %d ms from now, not about %d", d, delay)
	}
	// on cliairplay's clock: CLOCK_MONOTONIC_RAW plus the offset it was given, which no setting of the
	// wall clock moves
	_, off, _ := strings.Cut(string(args), "--clock-offset-ns\n")
	clock, err := strconv.ParseInt(strings.SplitN(off, "\n", 2)[0], 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	if d := startAt - (q2.RawNS()+clock)/1e6; d < 0 || d > delay+150 {
		t.Fatalf("starts %d ms from now on cliairplay's clock, not about %d", d, delay)
	}
	waitFor(t, "the mute", time.Second, func() bool { return tp.u32(offMute) == 1 && tp.u32(offState) == castOn })
	if tp.i32(offSpk) != 60 || tp.u32(offSpkSeq) != 1 {
		t.Fatalf("demo told %d (seq %d), not the speaker's 60", tp.i32(offSpk), tp.u32(offSpkSeq))
	}

	// The audio arrives whole and in order, in bursts.
	waitFor(t, "audio", 3*time.Second, func() bool { st, _ := os.Stat(filepath.Join(fake, "audio")); return st != nil && st.Size() > 4*tapRate })
	checkAudio := func() []uint16 {
		b, _ := os.ReadFile(filepath.Join(fake, "audio"))
		var s []uint16
		for i := 0; i+4 <= len(b); i += 4 {
			s = append(s, binary.LittleEndian.Uint16(b[i:]))
		}
		return s
	}
	s := checkAudio()
	for i := 1; i < len(s); i++ {
		if s[i] != s[i-1]+1 {
			t.Fatalf("frame %d: %d after %d", i, s[i], s[i-1])
		}
	}

	// The volume follows the Q2's wheel, once demo gives the speaker's: the speaker's own 60 back
	// is no change, 61 is.
	tp.set(offVolume, 60)
	time.Sleep(300 * time.Millisecond)
	if strings.Contains(strings.Join(commands(fake), "\n"), " VOLUME=") {
		t.Fatalf("a volume sent before the Q2 turned it: %v", commands(fake))
	}
	tp.set(offVolume, 61)
	waitFor(t, "VOLUME", time.Second, func() bool { return has(fake, "VOLUME=61") == 1 })
	if m, ok := e.takeLevels(); !ok || m["Kitchen"] != 61 {
		t.Fatalf("the speaker's level not kept: %v %v", m, ok)
	}

	// A pause: everything sent is heard, then the stream is flushed; Play starts it again from the next
	// frame, nothing lost.
	f.set(false)
	paused := f.t.u32(offSeq)
	waitFor(t, "the pause's FLUSH", 3*time.Second, func() bool { return has(fake, "ACTION=FLUSH") == 1 })
	if tp.u32(offMute) != 1 {
		t.Fatal("a pause unmuted the Q2")
	}
	time.Sleep(300 * time.Millisecond)
	f.set(true)
	waitFor(t, "the resume's START", 3*time.Second, func() bool { return has(fake, "ACTION=START") == 2 })
	waitFor(t, "audio after the pause", 3*time.Second, func() bool { return len(checkAudio()) > int(paused)+tapRate/2 })
	s = checkAudio()
	for i := 1; i < len(s); i++ {
		if s[i] != s[i-1]+1 {
			t.Fatalf("frame %d: %d after %d across the pause", i, s[i], s[i-1])
		}
	}

	// A seek: flushed at once and started again from the jump, the frames before it never sent.
	time.Sleep(500 * time.Millisecond)
	at := f.jump()
	waitFor(t, "the seek's START", 3*time.Second, func() bool { return has(fake, "ACTION=START") == 3 })
	if has(fake, "ACTION=FLUSH") != 2 {
		t.Fatalf("%v", commands(fake))
	}
	waitFor(t, "audio after the seek", 3*time.Second, func() bool {
		s := checkAudio()
		d := int(s[len(s)-1] - uint16(at)) // frames are numbered mod 65536
		return len(s) > 0 && d > tapRate/4 && d < 32768
	})
	// The stream breaks once, onto the jump, and runs on from it: frames are numbered mod 65536. (The
	// stand-in reads commands and audio apart, so where START fell in its audio is not exact.)
	s = checkAudio()
	breaks := 0
	for i := 1; i < len(s); i++ {
		if s[i] != s[i-1]+1 {
			breaks++
			if s[i] != uint16(at) {
				t.Fatalf("after the seek the stream goes on at %d, not the jump %d", s[i], uint16(at))
			}
		}
	}
	if breaks != 1 {
		t.Fatalf("%d breaks in the stream, not the seek's one", breaks)
	}

	// This Q2 again: the speaker let go and the Q2 aloud.
	e.choose(nil)
	waitFor(t, "STOP", 2*time.Second, func() bool { return has(fake, "ACTION=STOP") == 1 })
	if tp.u32(offMute) != 0 || tp.u32(offState) != castOff {
		t.Fatal("still muted after letting go")
	}
	close(stop)
	<-done
}

// TestNoDeadlock runs the main loop's second, the page's requests and a delay change against an
// engine sending: each must return. 1.0 hung within minutes when the main loop held the app's lock
// while asking the engine, which held its own while asking the app for the delay.
func TestNoDeadlock(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	os.Mkdir(fake, 0o755)
	t.Setenv("FAKE_CLIAIRPLAY", fake)
	tp, err := openTap(filepath.Join(dir, "q2cast"))
	if err != nil {
		t.Fatal(err)
	}
	f := &filter{t: tp, id: 1, stop: make(chan struct{})}
	defer close(f.stop)
	f.set(true)
	go f.run()
	c, err := net.ListenUDP("udp4", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	a := &app{dir: dir, started: time.Now(), br: &browser{services: map[string]*service{}, hosts: map[string]net.IP{},
		self: map[string]bool{}, conn: c, dest: c.LocalAddr().String()}} // queries to itself, not the network
	a.load()
	a.eng = newEngine(tp, engineOptions{bin: os.Args[0], cmdpipe: filepath.Join(dir, "cmd"), dacp: a.cfg.DACP,
		remote: a.cfg.Remote, track: filepath.Join(dir, "track"), levels: a.cfg.Volumes,
		delayMS: a.delay})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { a.eng.run(stop); close(done) }()
	defer func() { // a deadlocked engine never ends: don't wait for it
		close(stop)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
		}
	}()
	sp := speaker{Name: "Kitchen", IP: "127.0.0.1", Port: 1, AirPlay2: true} // refused: no /info, the saved level
	a.eng.choose(&sp)
	waitFor(t, "START", 3*time.Second, func() bool { return has(fake, "ACTION=START") == 1 })
	state := &q2.State{Path: filepath.Join(dir, "state")}
	end := time.Now().Add(1500 * time.Millisecond)
	var wg sync.WaitGroup
	for g := 0; g < 3; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for time.Now().Before(end) {
				switch g {
				case 0:
					a.second(time.Now(), tp, state)
				case 1:
					a.handle(q2spot.Request{Op: "menu"})
				case 2:
					a.handle(q2spot.Request{Op: "delay"})
				}
			}
		}(g)
	}
	finished := make(chan struct{})
	go func() { wg.Wait(); close(finished) }()
	select {
	case <-finished:
	case <-time.After(10 * time.Second):
		t.Fatal("deadlocked")
	}
	if b, _ := os.ReadFile(state.Path); !strings.Contains(string(b), "hold=1") {
		t.Fatalf("no hold while a speaker plays: %q", b)
	}
}

// TestStall starves the player while it plays: a short stall goes to the speaker as the same silence
// and the stream runs on, nothing lost or repeated; one too long to bridge starts the stream again
// from where it stood. Either way the speaker plays again once the player does.
func TestStall(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	os.Mkdir(fake, 0o755)
	t.Setenv("FAKE_CLIAIRPLAY", fake)
	tp, err := openTap(filepath.Join(dir, "q2cast"))
	if err != nil {
		t.Fatal(err)
	}
	f := &filter{t: tp, id: 1, stop: make(chan struct{})}
	defer close(f.stop)
	f.set(true)
	go f.run()
	e := newEngine(tp, engineOptions{bin: os.Args[0], cmdpipe: filepath.Join(dir, "cmd"), dacp: "0011223344556677",
		remote: "1", track: filepath.Join(dir, "track"), delayMS: func() int { return 400 }})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { e.run(stop); close(done) }()
	defer func() { close(stop); <-done }()
	sp := speaker{Name: "Kitchen", IP: "127.0.0.1", Port: 1, AirPlay2: true} // no /info, no saved level
	e.choose(&sp)
	waitFor(t, "START", 4*time.Second, func() bool { return has(fake, "ACTION=START") == 1 })
	if args, _ := os.ReadFile(filepath.Join(fake, "args")); !strings.Contains(string(args), "--volume\n35\n") || tp.i32(offSpk) != 35 {
		t.Fatalf("a speaker that says nothing starts at %d, not 35 (%q)", tp.i32(offSpk), args)
	}
	frames := func() []uint16 {
		b, _ := os.ReadFile(filepath.Join(fake, "audio"))
		s := make([]uint16, 0, len(b)/4)
		for i := 0; i+4 <= len(b); i += 4 {
			s = append(s, binary.LittleEndian.Uint16(b[i:]))
		}
		return s
	}
	stall := func(d time.Duration) uint32 {
		time.Sleep(600 * time.Millisecond)
		f.mu.Lock()
		f.stalled = true
		at := tp.u32(offSeq)
		f.mu.Unlock()
		time.Sleep(d)
		f.mu.Lock()
		f.stalled = false
		f.mu.Unlock()
		return at
	}
	// 1 s: the gap comes out as about 1 s of silence (frames of zeros), then the frame after the last
	at := stall(time.Second)
	waitFor(t, "audio after the short stall", 3*time.Second, func() bool {
		s := frames()
		return len(s) > 0 && int(s[len(s)-1]-uint16(at)) > tapRate/2 && int(s[len(s)-1]-uint16(at)) < 32768
	})
	if has(fake, "ACTION=FLUSH") != 0 {
		t.Fatalf("a 1 s stall flushed: %v", commands(fake))
	}
	if d := e.dropouts(time.Now()); !strings.HasPrefix(d, "1 dropout; the last, just now: the Q2's player stopped giving audio") {
		t.Fatalf("the page says %q", d)
	}
	s := frames()
	zeros, i := 0, 0
	for i = 1; i < len(s); i++ {
		if s[i] != s[i-1]+1 {
			break
		}
	}
	for ; i < len(s) && s[i] == 0 && s[i-1] != 0xffff; i++ {
		zeros++
	}
	if i >= len(s) || s[i] != uint16(at) || zeros < tapRate*8/10 || zeros > tapRate*13/10 {
		t.Fatalf("after the stall: %d frames of silence, then %d (want about %d, then %d)", zeros, s[min(i, len(s)-1)], tapRate, uint16(at))
	}
	// 6 s, past restartMS: flushed and started again from the frame where it stood
	at = stall(6 * time.Second)
	waitFor(t, "a new start after the long stall", 4*time.Second, func() bool { return has(fake, "ACTION=START") == 2 })
	if has(fake, "ACTION=FLUSH") != 1 {
		t.Fatalf("%v", commands(fake))
	}
	if d := e.dropouts(time.Now()); !strings.HasPrefix(d, "2 dropouts; the last, just now: the Q2's player stopped giving audio") {
		t.Fatalf("the page says %q", d)
	}
	waitFor(t, "audio after the long stall", 3*time.Second, func() bool {
		s := frames()
		return len(s) > 0 && int(s[len(s)-1]-uint16(at)) > tapRate/2 && int(s[len(s)-1]-uint16(at)) < 32768
	})
	s = frames()
	for i := 1; i < len(s); i++ { // frames only ever run on, or pause for silence: none lost or repeated
		if s[i] == 0 || s[i-1] == 0 {
			continue
		}
		if s[i] != s[i-1]+1 {
			t.Fatalf("frame %d: %d after %d", i, s[i], s[i-1])
		}
	}
}

func TestArt(t *testing.T) {
	dir := t.TempDir()
	jpeg := []byte{0xff, 0xd8, 0xff, 0xe0, 1, 2, 3}
	// a FLAC with a STREAMINFO block and a front cover
	pic := binary.BigEndian.AppendUint32(nil, 3)
	pic = binary.BigEndian.AppendUint32(pic, 10)
	pic = append(pic, "image/jpeg"...)
	pic = binary.BigEndian.AppendUint32(pic, 0)
	pic = append(pic, make([]byte, 16)...)
	pic = binary.BigEndian.AppendUint32(pic, uint32(len(jpeg)))
	pic = append(pic, jpeg...)
	flac := append([]byte("fLaC"), 0, 0, 0, 34)
	flac = append(flac, make([]byte, 34)...)
	flac = append(flac, 0x86, byte(len(pic)>>16), byte(len(pic)>>8), byte(len(pic)))
	flac = append(flac, pic...)
	song := filepath.Join(dir, "a.flac")
	os.WriteFile(song, flac, 0o644)
	out := filepath.Join(dir, "art")
	if findArt(song, out) != out {
		t.Fatal("no art from the FLAC")
	}
	if b, _ := os.ReadFile(out); string(b) != string(jpeg) {
		t.Fatalf("%x", b)
	}
	// a cover.jpg beside it wins
	png := []byte("\x89PNG\r\n\x1a\nxxxx")
	os.WriteFile(filepath.Join(dir, "Cover.PNG"), png, 0o644)
	if findArt(song, out) != out {
		t.Fatal("no art")
	}
	if b, _ := os.ReadFile(out); string(b) != string(png) {
		t.Fatalf("%x", b)
	}
	// an MP3's APIC (ID3v2.3)
	frame := append([]byte{0}, "image/jpeg\x00"...)
	frame = append(frame, 3)
	frame = append(frame, "front\x00"...)
	frame = append(frame, jpeg...)
	tag := append([]byte("APIC"), byte(len(frame)>>24), byte(len(frame)>>16), byte(len(frame)>>8), byte(len(frame)), 0, 0)
	tag = append(tag, frame...)
	n := len(tag)
	mp3 := append([]byte("ID3"), 3, 0, 0, byte(n>>21&127), byte(n>>14&127), byte(n>>7&127), byte(n&127))
	mp3 = append(mp3, tag...)
	other := t.TempDir()
	song = filepath.Join(other, "b.mp3")
	os.WriteFile(song, append(mp3, 0xff, 0xfb), 0o644)
	if findArt(song, out) != out {
		t.Fatal("no art from the MP3")
	}
	if b, _ := os.ReadFile(out); string(b) != string(jpeg) {
		t.Fatalf("%x", b)
	}
	if findArt(filepath.Join(other, "missing.flac"), out) != "" {
		t.Fatal("art for nothing")
	}
}

// heard is the audio the stand-in got: each frame's left sample.
func heard(fake string) []uint16 {
	b, _ := os.ReadFile(filepath.Join(fake, "audio"))
	s := make([]uint16, 0, len(b)/4)
	for i := 0; i+4 <= len(b); i += 4 {
		s = append(s, binary.LittleEndian.Uint16(b[i:]))
	}
	return s
}

// silenceBefore is how many frames of silence came between frame seq-1 and frame seq, the last time
// they came; -1 when they didn't.
func silenceBefore(s []uint16, seq uint32) int {
	for q := len(s) - 1; q > 0; q-- {
		if s[q] != uint16(seq) {
			continue
		}
		z := 0
		for q-1-z > 0 && s[q-1-z] == 0 {
			z++
		}
		if s[q-1-z] == uint16(seq-1) {
			return z
		}
	}
	return -1
}

// TestSongGap: the moment between two songs, the player draining one and opening the next, reaches
// the speaker as the same silence, however short (dropped, each song brought the speaker's place
// nearer, until Wi-Fi's hiccups broke it up); as short a gap within a song is the tap's unevenness,
// not silence. Neither is a dropout.
func TestSongGap(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	os.Mkdir(fake, 0o755)
	t.Setenv("FAKE_CLIAIRPLAY", fake)
	tp, err := openTap(filepath.Join(dir, "q2cast"))
	if err != nil {
		t.Fatal(err)
	}
	f := &filter{t: tp, id: 1, stop: make(chan struct{})}
	defer close(f.stop)
	f.set(true)
	go f.run()
	e := newEngine(tp, engineOptions{bin: os.Args[0], cmdpipe: filepath.Join(dir, "cmd"), dacp: "0011223344556677",
		remote: "1", track: filepath.Join(dir, "track"), delayMS: func() int { return 400 }})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { e.run(stop); close(done) }()
	defer func() { close(stop); <-done }()
	sp := speaker{Name: "Kitchen", IP: "127.0.0.1", Port: 1, AirPlay2: true}
	e.choose(&sp)
	waitFor(t, "START", 4*time.Second, func() bool { return has(fake, "ACTION=START") == 1 })
	const gap = 60 // ms
	// The player writes nothing for half a second, then (song's) frames at once, reaching back to gap
	// after its last: as it drains a song, then opens the next and fills its buffer. It goes on from
	// there at its pace once the engine has seen the fill (the stand-in's writes are a whole 100 ms at
	// once; the player's are spread over it, so the engine never dates the fill from a later one).
	stamp := func() int64 { return int64(uint64(tp.u32(offStamp)) | uint64(tp.u32(offStamp+4))<<32) }
	still := func(song uint32) uint32 {
		time.Sleep(700 * time.Millisecond)
		f.mu.Lock()
		f.stalled = true
		f.mu.Unlock()
		time.Sleep(500 * time.Millisecond)
		f.mu.Lock()
		at := tp.u32(offSeq)
		f.id = song
		f.block(int((q2.MonoNS()-stamp())*tapRate/1e9) - gap*tapRate/1000)
		f.mu.Unlock()
		time.Sleep(120 * time.Millisecond) // a tick of the engine's
		f.mu.Lock()
		f.block(int((q2.MonoNS() - stamp()) * tapRate / 1e9))
		f.stalled = false
		f.mu.Unlock()
		return at
	}
	after := func(at uint32) {
		waitFor(t, "audio after the gap", 3*time.Second, func() bool {
			s := heard(fake)
			return len(s) > 0 && int(s[len(s)-1]-uint16(at)) > tapRate/2 && int(s[len(s)-1]-uint16(at)) < 32768
		})
	}
	before := e.dropouts(time.Now())
	at := still(2)
	after(at)
	if z := silenceBefore(heard(fake), at); z < gap*tapRate/1000*3/4 || z > gap*tapRate/1000*5/4 {
		t.Fatalf("%d frames of silence between the songs, not about %d", z, gap*tapRate/1000)
	}
	at = still(2)
	after(at)
	if z := silenceBefore(heard(fake), at); z != 0 {
		t.Fatalf("%d frames of silence within a song", z)
	}
	if d := e.dropouts(time.Now()); d != before {
		t.Fatalf("the page says %q", d)
	}
}

// TestReconnect: cliairplay ending itself, parked 2 min, is no dropout: it's connected again at once,
// still over AirPlay 2. A connection lost while playing is a dropout, tried again the same way (1.5 went
// to AirPlay 1, a real-time stream that Wi-Fi breaks up). AirPlay 1 comes only when AirPlay 2 can't
// connect, and stays while it works.
func TestReconnect(t *testing.T) {
	dir := t.TempDir()
	fake := filepath.Join(dir, "fake")
	os.Mkdir(fake, 0o755)
	t.Setenv("FAKE_CLIAIRPLAY", fake)
	tp, err := openTap(filepath.Join(dir, "q2cast"))
	if err != nil {
		t.Fatal(err)
	}
	f := &filter{t: tp, id: 1, stop: make(chan struct{})}
	defer close(f.stop)
	f.set(true)
	go f.run()
	e := newEngine(tp, engineOptions{bin: os.Args[0], cmdpipe: filepath.Join(dir, "cmd"), dacp: "0011223344556677",
		remote: "1", track: filepath.Join(dir, "track"), delayMS: func() int { return 400 }})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { e.run(stop); close(done) }()
	defer func() { close(stop); <-done }()
	protocols := func() []string {
		b, _ := os.ReadFile(filepath.Join(fake, "starts"))
		var p []string
		for _, line := range strings.Split(strings.TrimSpace(string(b)), "\n") {
			_, rest, _ := strings.Cut(line, "--protocol ")
			p = append(p, strings.Fields(rest)[0])
		}
		return p
	}
	runs := func(n int, why string) []string {
		t.Helper()
		waitFor(t, why, 6*time.Second, func() bool { return len(protocols()) == n })
		return protocols()
	}
	tell := func(line string) { // to the running stand-in, on its command pipe
		p, err := os.OpenFile(filepath.Join(dir, "cmd"), os.O_WRONLY, 0)
		if err != nil {
			t.Fatal(err)
		}
		p.WriteString(line + "\n")
		p.Close()
	}
	started := func() { waitFor(t, "START", 4*time.Second, func() bool { return has(fake, "ACTION=START") == 1 }) }
	sp := speaker{Name: "Kitchen", IP: "127.0.0.1", Port: 1, AirPlay2: true}
	e.choose(&sp)
	started()

	f.set(false)
	waitFor(t, "the pause's FLUSH", 3*time.Second, func() bool { return has(fake, "ACTION=FLUSH") == 1 })
	tell("TEST=IDLE")
	if p := runs(2, "a new connection after the idle end"); p[1] != "auto" {
		t.Fatalf("after the idle end: %v", p)
	}
	if d := e.dropouts(time.Now()); d != "" {
		t.Fatalf("the idle end noted: %q", d)
	}
	f.set(true)
	started()

	tell("TEST=EXIT")
	if p := runs(3, "a new connection after the loss"); p[2] != "auto" {
		t.Fatalf("after a lost connection: %v", p)
	}
	if d := e.dropouts(time.Now()); !strings.HasPrefix(d, "1 dropout; the last, just now: the connection to Kitchen ended") {
		t.Fatalf("the page says %q", d)
	}
	started()

	os.WriteFile(filepath.Join(fake, "fail-auto"), nil, 0o644) // AirPlay 2 can't connect: AirPlay 1 at once
	tell("TEST=EXIT")
	if p := runs(5, "AirPlay 1 after AirPlay 2 failed"); p[3] != "auto" || p[4] != "airplay2-compat" {
		t.Fatalf("%v", p)
	}
	started()
	os.Remove(filepath.Join(fake, "fail-auto"))
	tell("TEST=EXIT") // AirPlay 1 worked: tried again
	if p := runs(6, "a new connection after AirPlay 1's loss"); p[5] != "airplay2-compat" {
		t.Fatalf("%v", p)
	}
}
