package main

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"net/textproto"
	"strconv"
	"strings"
	"time"
)

// A speaker's own volume, before connecting: AirPlay 2 receivers give it as initialVolume (dB) in
// GET /info's binary plist, the plain request cliairplay itself starts with. AirPlay's 0-100 is
// -30..0 dB, linear (libraop's raopcl_float_volume), 0 muted, so level is the exact inverse.

const infoTimeout = 2 * time.Second

// speakerLevel is the speaker's volume, 0-100, and whether it said.
func speakerLevel(ip string, port int, dacp, remote string) (int, bool) {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(ip, strconv.Itoa(port)), infoTimeout)
	if err != nil {
		return 0, false
	}
	defer c.Close()
	c.SetDeadline(time.Now().Add(infoTimeout))
	fmt.Fprintf(c, "GET /info RTSP/1.0\r\nCSeq: 1\r\nUser-Agent: AirPlay/670.6.2\r\nDACP-ID: %s\r\n"+
		"Active-Remote: %s\r\nContent-Length: 0\r\n\r\n", dacp, remote)
	r := textproto.NewReader(bufio.NewReader(c))
	status, err := r.ReadLine()
	if err != nil || !strings.HasPrefix(status, "RTSP/1.0 200") {
		return 0, false
	}
	h, err := r.ReadMIMEHeader()
	if err != nil {
		return 0, false
	}
	n, err := strconv.Atoi(h.Get("Content-Length"))
	if err != nil || n <= 0 || n > 1<<20 {
		return 0, false
	}
	body := make([]byte, n)
	if _, err := io.ReadFull(r.R, body); err != nil {
		return 0, false
	}
	db, ok := plistNumber(body, "initialVolume")
	if !ok {
		return 0, false
	}
	return level(db), true
}

// level is AirPlay dB as 0-100: -30..0 linear, at or under -30 (and -144, muted) 0.
func level(db float64) int {
	if math.IsNaN(db) || db <= -30 {
		return 0
	}
	if db >= 0 {
		return 100
	}
	return int(math.Round((db + 30) * 100 / 30))
}

var errPlist = errors.New("bplist: malformed")

// plistNumber is the number a binary plist's top dictionary holds under key (an int or a real).
func plistNumber(b []byte, key string) (float64, bool) {
	if len(b) < 40 || string(b[:8]) != "bplist00" {
		return 0, false
	}
	t := b[len(b)-32:]
	offSize, refSize := int(t[6]), int(t[7])
	count, top, table := binary.BigEndian.Uint64(t[8:]), binary.BigEndian.Uint64(t[16:]), binary.BigEndian.Uint64(t[24:])
	if offSize < 1 || offSize > 8 || refSize < 1 || refSize > 8 || count > 1<<20 || top >= count ||
		table+count*uint64(offSize) > uint64(len(b)-32) {
		return 0, false
	}
	uintAt := func(at, size int) (uint64, error) {
		if at < 0 || at+size > len(b) {
			return 0, errPlist
		}
		var v uint64
		for i := 0; i < size; i++ {
			v = v<<8 | uint64(b[at+i])
		}
		return v, nil
	}
	object := func(ref uint64) (int, error) { // its offset
		if ref >= count {
			return 0, errPlist
		}
		o, err := uintAt(int(table)+int(ref)*offSize, offSize)
		if err != nil || o >= table {
			return 0, errPlist
		}
		return int(o), nil
	}
	// length is an object's count: its marker's low nibble, or the int that follows a 0xf.
	length := func(at int) (n, data int, err error) {
		n, data = int(b[at]&15), at+1
		if n != 15 {
			return n, data, nil
		}
		if data >= len(b) || b[data]>>4 != 1 {
			return 0, 0, errPlist
		}
		size := 1 << (b[data] & 15)
		v, err := uintAt(data+1, size)
		if err != nil || v > 1<<20 {
			return 0, 0, errPlist
		}
		return int(v), data + 1 + size, nil
	}
	at, err := object(top)
	if err != nil || b[at]>>4 != 0xd {
		return 0, false
	}
	n, refs, err := length(at)
	if err != nil {
		return 0, false
	}
	for i := 0; i < n; i++ {
		kr, err1 := uintAt(refs+i*refSize, refSize)
		vr, err2 := uintAt(refs+(n+i)*refSize, refSize)
		if err1 != nil || err2 != nil {
			return 0, false
		}
		ko, err := object(kr)
		if err != nil || b[ko]>>4 != 5 { // an ASCII string
			continue
		}
		kn, kd, err := length(ko)
		if err != nil || kd+kn > len(b) || string(b[kd:kd+kn]) != key {
			continue
		}
		vo, err := object(vr)
		if err != nil {
			return 0, false
		}
		size := 1 << (b[vo] & 15)
		v, err := uintAt(vo+1, size)
		if err != nil {
			return 0, false
		}
		switch b[vo] >> 4 {
		case 1: // an int, signed only at 8 bytes
			if size == 8 {
				return float64(int64(v)), true
			}
			return float64(v), true
		case 2: // a real
			if size == 4 {
				return float64(math.Float32frombits(uint32(v))), true
			}
			if size == 8 {
				return math.Float64frombits(v), true
			}
		}
		return 0, false
	}
	return 0, false
}
