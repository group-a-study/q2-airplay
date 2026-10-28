package main

import (
	"bufio"
	"encoding/binary"
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
)

// TestMain runs as a stand-in cliairplay when FAKE_CLIAIRPLAY names a directory: it records its
// commands and its stdin there and answers as the real one does (DESIGN.md's [STATUS] lines).
func TestMain(m *testing.M) {
	if dir := os.Getenv("FAKE_CLIAIRPLAY"); dir != "" {
		fakeSender(dir, os.Args[1:])
		os.Exit(0)
	}
	os.Exit(m.Run())
}

func fakeSender(dir string, args []string) {
	pipe := ""
	for i, a := range args {
		if a == "--cmdpipe" {
			pipe = args[i+1]
		}
	}
	os.WriteFile(filepath.Join(dir, "args"), []byte(strings.Join(args, "\n")), 0o644)
	audio, _ := os.Create(filepath.Join(dir, "audio"))
	var mu sync.Mutex
	marks, _ := os.Create(filepath.Join(dir, "commands"))
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
		return
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
			return
		}
	}
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
		if f.playing {
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
	os.WriteFile(filepath.Join(dir, "config.ini"), []byte("[PLAYSET]\nVOLUME=50\n[Q2POD]\nAIRVOL=42\n"), 0o644)
	tp.set(offVolume, 0xffffffff) // demo's -1 until the speaker plays: nothing sent

	f := &filter{t: tp, id: 1, stop: make(chan struct{})}
	defer close(f.stop)
	f.set(true)
	go f.run()
	const delay = 400
	e := newEngine(tp, engineOptions{bin: os.Args[0], cmdpipe: filepath.Join(dir, "cmd"), dacp: "0011223344556677",
		remote: "1", track: filepath.Join(dir, "track"), config: filepath.Join(dir, "config.ini"),
		delayMS: func() int { return delay }})
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() { e.run(stop); close(done) }()
	sp := speaker{Name: "Kitchen", IP: "10.0.0.5", Port: 7000, TXT: "features=0x445F8A00,0x1C340", AirPlay2: true}
	e.choose(&sp)

	// Connected, then started from what plays: the Q2 muted, demo told it plays.
	waitFor(t, "START", 3*time.Second, func() bool { return has(fake, "ACTION=START") == 1 })
	args, _ := os.ReadFile(filepath.Join(fake, "args"))
	for _, want := range []string{"--protocol\nauto", "--volume\n42", "--txt\nfeatures=0x445F8A00,0x1C340", "--latency\n900", "10.0.0.5"} {
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
	waitFor(t, "the mute", time.Second, func() bool { return tp.u32(offMute) == 1 && tp.u32(offState) == castOn })

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

	// The volume follows the Q2's wheel, once demo gives the speaker's.
	if strings.Contains(strings.Join(commands(fake), "\n"), " VOLUME=") {
		t.Fatalf("a volume sent before demo gave the speaker's: %v", commands(fake))
	}
	tp.set(offVolume, 60)
	waitFor(t, "VOLUME", time.Second, func() bool { return has(fake, "VOLUME=60") == 1 })

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
		return len(s) > 0 && int32(uint32(s[len(s)-1])-at&0xffff) > tapRate/4
	})
	// the stream after the last START begins at the jump (frames are numbered mod 65536)
	var startBytes int
	for _, c := range commands(fake) {
		if strings.HasSuffix(c, " ACTION=START") {
			startBytes, _ = strconv.Atoi(strings.Fields(c)[0])
		}
	}
	s = checkAudio()
	if first := s[startBytes/4]; first != uint16(at) {
		t.Fatalf("after the seek the stream starts at %d, not the jump %d", first, uint16(at))
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

func TestSavedVolume(t *testing.T) {
	p := filepath.Join(t.TempDir(), "config.ini")
	if savedVolume(p) != 35 {
		t.Fatal("default")
	}
	os.WriteFile(p, []byte("[PLAYSET]\nAIRVOL=80\n[Q2POD]\nMONO=0\nAIRVOL = 23\n"), 0o644)
	if savedVolume(p) != 23 {
		t.Fatal(savedVolume(p))
	}
}
