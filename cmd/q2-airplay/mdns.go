package main

import (
	"encoding/binary"
	"errors"
	"math/rand"
	"net"
	"sort"
	"strings"
	"sync"
	"time"
)

// AirPlay speakers on the Wi-Fi, by DNS-SD over multicast DNS (RFC 6762, 6763): _airplay._tcp
// (AirPlay 2, and AirPlay 1 speakers that advertise it too) and _raop._tcp (AirPlay 1). Queries go
// from an ephemeral port, so responders answer by unicast to it (RFC 6762 6.7) and the Q2's own
// responder (stock's AirPlay receiver) keeps port 5353. A speaker is the merge of its two services.

const (
	mdnsAddr     = "224.0.0.251:5353"
	airplayType  = "_airplay._tcp.local."
	raopType     = "_raop._tcp.local."
	dnsA         = 1
	dnsPTR       = 12
	dnsTXT       = 16
	dnsSRV       = 33
	speakerStale = 3 * time.Minute // not heard again this long: gone
)

type service struct {
	instance string // "Kitchen._airplay._tcp.local."
	target   string // "Sonos-1234.local."
	port     int
	txt      []string
	seen     time.Time
}

// speaker is what the page lists and cliairplay is given.
type speaker struct {
	Name     string
	IP       string
	Port     int    // the RTSP port: _airplay's, else _raop's
	TXT      string // _airplay's TXT, "k=v k=v", for cliairplay's --txt
	RaopTXT  map[string]string
	Model    string
	AirPlay2 bool // an _airplay._tcp service
}

func (s speaker) key() string { return s.Name + "@" + s.IP }

type browser struct {
	mu       sync.Mutex
	services map[string]*service // by instance
	hosts    map[string]net.IP   // A records, by name
	self     map[string]bool     // this device's addresses
	conn     *net.UDPConn
	dest     string // where queries go: mdnsAddr
}

func newBrowser() (*browser, error) {
	c, err := net.ListenUDP("udp4", &net.UDPAddr{})
	if err != nil {
		return nil, err
	}
	b := &browser{services: map[string]*service{}, hosts: map[string]net.IP{}, conn: c, dest: mdnsAddr}
	go b.read()
	return b, nil
}

// query asks for the speakers, and for the records a service still lacks.
func (b *browser) query() {
	qs := []question{{airplayType, dnsPTR}, {raopType, dnsPTR}}
	b.mu.Lock()
	for _, s := range b.services {
		if s.target == "" || s.txt == nil {
			qs = append(qs, question{s.instance, dnsSRV}, question{s.instance, dnsTXT})
		} else if b.hosts[strings.ToLower(s.target)] == nil {
			qs = append(qs, question{s.target, dnsA})
		}
	}
	b.mu.Unlock()
	to, err := net.ResolveUDPAddr("udp4", b.dest)
	if err != nil || b.conn == nil {
		return
	}
	for len(qs) > 0 { // a few questions a packet
		n := len(qs)
		if n > 8 {
			n = 8
		}
		b.conn.WriteToUDP(encodeQuery(uint16(rand.Intn(65536)), qs[:n]), to)
		qs = qs[n:]
	}
}

func (b *browser) read() {
	buf := make([]byte, 9000)
	for {
		n, _, err := b.conn.ReadFromUDP(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			continue
		}
		rrs, err := parseMessage(buf[:n])
		if err != nil {
			continue
		}
		b.take(rrs, time.Now())
	}
}

func (b *browser) take(rrs []record, now time.Time) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, r := range rrs {
		name := strings.ToLower(r.name)
		switch r.typ {
		case dnsPTR:
			if name != airplayType && name != raopType {
				continue
			}
			if r.ttl == 0 {
				delete(b.services, strings.ToLower(r.ptr))
				continue
			}
			s := b.service(r.ptr)
			s.seen = now
		case dnsSRV:
			if s, ok := b.services[name]; ok {
				s.target, s.port, s.seen = r.target, r.port, now
			}
		case dnsTXT:
			if s, ok := b.services[name]; ok {
				s.txt, s.seen = r.txt, now
			}
		case dnsA:
			if r.ttl == 0 {
				delete(b.hosts, name)
			} else {
				b.hosts[name] = r.ip
			}
		}
	}
	// a SRV or TXT can come before its PTR in one packet: take them again now the service exists
	for _, r := range rrs {
		s, ok := b.services[strings.ToLower(r.name)]
		if !ok {
			continue
		}
		if r.typ == dnsSRV && s.target == "" {
			s.target, s.port = r.target, r.port
		}
		if r.typ == dnsTXT && s.txt == nil {
			s.txt = r.txt
		}
	}
}

func (b *browser) service(instance string) *service {
	k := strings.ToLower(instance)
	s := b.services[k]
	if s == nil {
		s = &service{instance: instance}
		b.services[k] = s
	}
	return s
}

// speakers is every speaker heard lately with an address, by name: an _airplay._tcp service with
// its _raop._tcp one's TXT when both are there; this device's own receiver left out.
func (b *browser) speakers(now time.Time) []speaker {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.self == nil {
		b.self = localAddrs()
	}
	byIP := map[string]*speaker{}
	for k, s := range b.services {
		if now.Sub(s.seen) > speakerStale {
			delete(b.services, k)
			continue
		}
		ip := b.hosts[strings.ToLower(s.target)]
		if ip == nil || s.port == 0 || s.txt == nil || b.self[ip.String()] {
			continue
		}
		airplay := strings.HasSuffix(k, "."+airplayType)
		name := instanceName(s.instance, airplay)
		sp := byIP[ip.String()+"/"+strings.ToLower(name)]
		if sp == nil {
			sp = &speaker{Name: name, IP: ip.String()}
			byIP[ip.String()+"/"+strings.ToLower(name)] = sp
		}
		txt := txtMap(s.txt)
		if airplay {
			sp.AirPlay2, sp.Port = true, s.port
			sp.TXT = strings.Join(s.txt, " ")
			if m := txt["model"]; m != "" {
				sp.Model = m
			}
		} else {
			sp.RaopTXT = txt
			if !sp.AirPlay2 {
				sp.Port = s.port
			}
			if m := txt["am"]; m != "" && sp.Model == "" {
				sp.Model = m
			}
		}
	}
	out := make([]speaker, 0, len(byIP))
	for _, sp := range byIP {
		out = append(out, *sp)
	}
	sort.Slice(out, func(i, j int) bool {
		a, c := strings.ToLower(out[i].Name), strings.ToLower(out[j].Name)
		if a != c {
			return a < c
		}
		return out[i].IP < out[j].IP
	})
	return out
}

// instanceName is the service instance's label: _raop's "001122AABBCC@Kitchen" is "Kitchen".
func instanceName(instance string, airplay bool) string {
	typ := airplayType
	if !airplay {
		typ = raopType
	}
	name := instance
	if len(name) > len(typ)+1 && strings.EqualFold(name[len(name)-len(typ)-1:], "."+typ) {
		name = name[:len(name)-len(typ)-1]
	}
	name = strings.ReplaceAll(name, `\.`, ".")
	if !airplay {
		if i := strings.IndexByte(name, '@'); i >= 0 {
			name = name[i+1:]
		}
	}
	return name
}

func txtMap(txt []string) map[string]string {
	m := map[string]string{}
	for _, kv := range txt {
		if i := strings.IndexByte(kv, '='); i > 0 {
			m[strings.ToLower(kv[:i])] = kv[i+1:]
		}
	}
	return m
}

func localAddrs() map[string]bool {
	m := map[string]bool{}
	addrs, _ := net.InterfaceAddrs()
	for _, a := range addrs {
		if n, ok := a.(*net.IPNet); ok {
			m[n.IP.String()] = true
		}
	}
	return m
}

// DNS messages: just what DNS-SD browsing needs.

type question struct {
	name string
	typ  uint16
}

type record struct {
	name   string
	typ    uint16
	ttl    uint32
	ptr    string   // PTR
	target string   // SRV
	port   int      // SRV
	txt    []string // TXT
	ip     net.IP   // A
}

// encodeName writes name's labels; a "\." inside one (as readName gives it) is a dot of the label.
func encodeName(b []byte, name string) []byte {
	var label []byte
	flush := func() {
		if len(label) > 0 && len(label) < 64 {
			b = append(b, byte(len(label)))
			b = append(b, label...)
		}
		label = label[:0]
	}
	for i := 0; i < len(name); i++ {
		switch {
		case name[i] == '\\' && i+1 < len(name) && name[i+1] == '.':
			label = append(label, '.')
			i++
		case name[i] == '.':
			flush()
		default:
			label = append(label, name[i])
		}
	}
	flush()
	return append(b, 0)
}

func encodeQuery(id uint16, qs []question) []byte {
	b := make([]byte, 12, 512)
	binary.BigEndian.PutUint16(b, id)
	binary.BigEndian.PutUint16(b[4:], uint16(len(qs)))
	for _, q := range qs {
		b = encodeName(b, q.name)
		b = binary.BigEndian.AppendUint16(b, q.typ)
		b = binary.BigEndian.AppendUint16(b, 1) // IN
	}
	return b
}

var errShort = errors.New("dns: short message")

// readName reads a (possibly compressed) name at off; the offset past it in the message.
func readName(m []byte, off int) (string, int, error) {
	var labels []string
	end, jumps := -1, 0
	for {
		if off >= len(m) {
			return "", 0, errShort
		}
		n := int(m[off])
		switch {
		case n == 0:
			if end < 0 {
				end = off + 1
			}
			return strings.Join(labels, ".") + ".", end, nil
		case n&0xc0 == 0xc0:
			if off+1 >= len(m) || jumps > 20 {
				return "", 0, errShort
			}
			if end < 0 {
				end = off + 2
			}
			off = int(binary.BigEndian.Uint16(m[off:]) & 0x3fff)
			jumps++
		default:
			if off+1+n > len(m) {
				return "", 0, errShort
			}
			labels = append(labels, strings.ReplaceAll(string(m[off+1:off+1+n]), ".", `\.`))
			off += 1 + n
		}
	}
}

// parseMessage returns a response's answers, authority and additional records.
func parseMessage(m []byte) ([]record, error) {
	if len(m) < 12 || m[2]&0x80 == 0 {
		return nil, errors.New("dns: not a response")
	}
	qd := int(binary.BigEndian.Uint16(m[4:]))
	rr := int(binary.BigEndian.Uint16(m[6:])) + int(binary.BigEndian.Uint16(m[8:])) + int(binary.BigEndian.Uint16(m[10:]))
	off := 12
	for i := 0; i < qd; i++ {
		_, next, err := readName(m, off)
		if err != nil {
			return nil, err
		}
		off = next + 4
	}
	var out []record
	for i := 0; i < rr; i++ {
		name, next, err := readName(m, off)
		if err != nil || next+10 > len(m) {
			return out, errShort
		}
		r := record{name: name, typ: binary.BigEndian.Uint16(m[next:]), ttl: binary.BigEndian.Uint32(m[next+4:])}
		n := int(binary.BigEndian.Uint16(m[next+8:]))
		data := next + 10
		if data+n > len(m) {
			return out, errShort
		}
		rd := m[data : data+n]
		switch r.typ {
		case dnsPTR:
			r.ptr, _, err = readName(m, data)
		case dnsSRV:
			if n >= 7 {
				r.port = int(binary.BigEndian.Uint16(rd[4:]))
				r.target, _, err = readName(m, data+6)
			}
		case dnsTXT:
			r.txt = []string{}
			for j := 0; j < len(rd); {
				l := int(rd[j])
				if j+1+l > len(rd) {
					break
				}
				if l > 0 {
					r.txt = append(r.txt, string(rd[j+1:j+1+l]))
				}
				j += 1 + l
			}
		case dnsA:
			if n == 4 {
				r.ip = net.IPv4(rd[0], rd[1], rd[2], rd[3])
			}
		}
		if err == nil {
			out = append(out, r)
		}
		off = data + n
	}
	return out, nil
}
