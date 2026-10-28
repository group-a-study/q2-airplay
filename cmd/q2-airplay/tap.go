package main

import (
	"encoding/binary"
	"errors"
	"os"
	"sync/atomic"
	"syscall"
	"unsafe"

	"github.com/group-a-study/q2-airplay/internal/q2"
)

// The tap: q2-pod's cast_tap (patch/peq.h), a file in /tmp this app makes and keeps, mapped by
// hciplayer's PEQ filter (what plays, at tapRate, into the ring) and by demo (the player's state, the
// volume and CAST_TRACK's count). Fields are read and written by their offsets: Go's 32-bit MIPS
// layout aligns an int64 to 4 bytes, the payload's to 8.
const (
	tapFile   = "/tmp/q2cast"
	trackFile = "/tmp/q2cast.track"
	tapMagic  = 0x31544351 // "QCT1"
	tapRate   = 44100
	tapRing   = 262144 // frames, a power of two: 5.9 s at tapRate
	tapSize   = 160 + tapRing*4

	offMagic, offSize              = 0, 4
	offWant, offMute, offState     = 8, 12, 16
	offPosMS, offPosID, offPosAt   = 20, 24, 28
	offPosRun, offSpeaker          = 32, 36
	offSeq, offID, offPTS          = 100, 104, 108
	offJumps, offJumpSeq, offDSD   = 112, 116, 120
	offSpk, offSpkSeq              = 124, 156 // cast_tap's pad0, pad1: the speaker's volume for demo
	offStamp                       = 128
	offBeat, offStatus             = 136, 140
	offVolume, offMaxVol, offTrack = 144, 148, 152
	offRing                        = 160
	speakerMax                     = 64
)

// The app's state for demo (CAST_*).
const (
	castOff = iota
	castConnecting
	castOn
)

// Player statuses, mclGetPlayStatus's.
const (
	playStopped = 1
	playPlaying = 2
	playPaused  = 3
)

type tap struct{ mem []byte }

// openTap maps the tap file, made whole under another name first when it isn't there or isn't this
// layout, so neither reader ever maps a short one.
func openTap(path string) (*tap, error) {
	if t, err := mapTap(path); err == nil {
		return t, nil
	}
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0o666)
	if err != nil {
		return nil, err
	}
	head := make([]byte, 8)
	binary.LittleEndian.PutUint32(head, tapMagic)
	binary.LittleEndian.PutUint32(head[4:], tapSize)
	_, err = f.Write(head)
	if err == nil {
		err = f.Truncate(tapSize)
	}
	f.Close()
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
		return nil, err
	}
	return mapTap(path)
}

func mapTap(path string) (*tap, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	if st, err := f.Stat(); err != nil || st.Size() != tapSize {
		return nil, errors.New("tap: not this layout")
	}
	mem, err := syscall.Mmap(int(f.Fd()), 0, tapSize, syscall.PROT_READ|syscall.PROT_WRITE, syscall.MAP_SHARED)
	if err != nil {
		return nil, err
	}
	t := &tap{mem}
	if t.u32(offMagic) != tapMagic || t.u32(offSize) != tapSize {
		syscall.Munmap(mem)
		return nil, errors.New("tap: not this layout")
	}
	return t, nil
}

func (t *tap) ptr(off int) *uint32 { return (*uint32)(unsafe.Pointer(&t.mem[off])) }
func (t *tap) u32(off int) uint32  { return atomic.LoadUint32(t.ptr(off)) }
func (t *tap) i32(off int) int32   { return int32(t.u32(off)) }
func (t *tap) set(off int, v uint32) {
	atomic.StoreUint32(t.ptr(off), v)
}

// clock is the payload's CAST_CLOCK: CLOCK_MONOTONIC in 1.048576 ms ticks.
func clock(ns int64) uint32 { return uint32(uint64(ns) >> 20) }

// beat tells the filter and demo the app reads; mute silences the Q2's own output.
func (t *tap) beat(now int64, mute bool, state int) {
	m := uint32(0)
	if mute {
		m = 1
	}
	t.set(offMute, m)
	t.set(offState, uint32(state))
	t.set(offWant, clock(now))
}

// stop lets go: the Q2 plays aloud again at once.
func (t *tap) stop() {
	t.set(offMute, 0)
	t.set(offState, castOff)
	t.set(offWant, clock(q2.MonoNS())-1<<20) // long stale
}

// setLevel tells demo the speaker's volume (0-100), to show and turn from: the level, then a new
// seq (0 means never told: an older app's padding).
func (t *tap) setLevel(v int) {
	t.set(offSpk, uint32(int32(v)))
	seq := t.u32(offSpkSeq) + 1
	if seq == 0 {
		seq = 1
	}
	t.set(offSpkSeq, seq)
}

func (t *tap) setSpeaker(name string) {
	b := make([]byte, speakerMax)
	copy(b[:speakerMax-1], name)
	copy(t.mem[offSpeaker:offSpeaker+speakerMax], b)
}

// position tells demo where the speaker is (for the lyrics): ms into the song of filter instance id,
// as of now, running on or not. at last, as demo reads it first.
func (t *tap) position(ms int32, id uint32, run bool, now int64) {
	r := uint32(0)
	if run {
		r = 1
	}
	t.set(offPosMS, uint32(ms))
	t.set(offPosID, id)
	t.set(offPosRun, r)
	t.set(offPosAt, clock(now))
}

// head is a consistent read of what the filter wrote last: seq is read last on its side, so the
// fields before it are at least as new.
type head struct {
	seq, id, jumps, jumpSeq uint32
	pts                     int32
	stamp                   int64
	dsd                     bool
}

func (t *tap) head() head {
	var h head
	for i := 0; i < 3; i++ {
		h.seq = t.u32(offSeq)
		h.id = t.u32(offID)
		h.pts = t.i32(offPTS)
		h.stamp = int64(uint64(t.u32(offStamp)) | uint64(t.u32(offStamp+4))<<32)
		h.jumps = t.u32(offJumps)
		h.jumpSeq = t.u32(offJumpSeq)
		h.dsd = t.u32(offDSD) != 0
		if t.u32(offSeq) == h.seq {
			break
		}
	}
	return h
}

// frames copies the ring's frames [from, from+n) as 16-bit little-endian stereo into out.
func (t *tap) frames(out []byte, from uint32, n int) []byte {
	for n > 0 {
		at := int(from & (tapRing - 1))
		k := tapRing - at
		if k > n {
			k = n
		}
		out = append(out, t.mem[offRing+4*at:offRing+4*(at+k)]...)
		from += uint32(k)
		n -= k
	}
	return out
}

func (t *tap) status() int32 { return t.i32(offStatus) }
func (t *tap) volume() int32 { return t.i32(offVolume) }
func (t *tap) track() uint32 { return t.u32(offTrack) }
