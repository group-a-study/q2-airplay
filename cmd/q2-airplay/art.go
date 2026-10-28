package main

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// A song's cover for the speaker's now-playing: a picture in its folder named as covers are, else the
// one in the file itself (FLAC's PICTURE block, an ID3v2 APIC frame), copied to out. "" without one.

const artMax = 4 << 20 // cliairplay takes up to 5 MiB

var coverNames = []string{"cover", "folder", "front", "album", "albumart", "albumartsmall"}

func findArt(song, out string) string {
	if pic := folderArt(filepath.Dir(song)); pic != "" {
		if b, err := readAll(pic, artMax); err == nil && imageType(b) != "" {
			return writeArt(out, b)
		}
	}
	if b := embeddedArt(song); imageType(b) != "" {
		return writeArt(out, b)
	}
	return ""
}

func folderArt(dir string) string {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return ""
	}
	best, rank := "", len(coverNames)
	for _, e := range entries {
		name := strings.ToLower(e.Name())
		ext := filepath.Ext(name)
		if e.IsDir() || (ext != ".jpg" && ext != ".jpeg" && ext != ".png") {
			continue
		}
		for i, c := range coverNames {
			if strings.TrimSuffix(name, ext) == c && i < rank {
				best, rank = filepath.Join(dir, e.Name()), i
			}
		}
	}
	return best
}

func writeArt(out string, b []byte) string {
	if os.WriteFile(out, b, 0o644) != nil {
		return ""
	}
	return out
}

func imageType(b []byte) string {
	switch {
	case len(b) > 3 && b[0] == 0xff && b[1] == 0xd8:
		return "image/jpeg"
	case len(b) > 8 && bytes.HasPrefix(b, []byte("\x89PNG\r\n\x1a\n")):
		return "image/png"
	}
	return ""
}

func readAll(path string, max int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(io.LimitReader(f, max))
}

// embeddedArt is the front cover (else the first picture) in a FLAC or MP3's tags.
func embeddedArt(song string) []byte {
	f, err := os.Open(song)
	if err != nil {
		return nil
	}
	defer f.Close()
	head := make([]byte, 10)
	if _, err := io.ReadFull(f, head); err != nil {
		return nil
	}
	switch {
	case bytes.HasPrefix(head, []byte("fLaC")):
		return flacPicture(f)
	case bytes.HasPrefix(head, []byte("ID3")):
		return id3Picture(f, head)
	}
	return nil
}

// flacPicture walks the metadata blocks after "fLaC" (f is 10 bytes in: past the marker and the
// first block's header and 2 bytes of it).
func flacPicture(f *os.File) []byte {
	if _, err := f.Seek(4, io.SeekStart); err != nil {
		return nil
	}
	var first []byte
	for i := 0; i < 64; i++ {
		var h [4]byte
		if _, err := io.ReadFull(f, h[:]); err != nil {
			return first
		}
		last, typ := h[0]&0x80 != 0, h[0]&0x7f
		n := int64(h[1])<<16 | int64(h[2])<<8 | int64(h[3])
		if typ == 6 && n <= artMax+1024 {
			b := make([]byte, n)
			if _, err := io.ReadFull(f, b); err != nil {
				return first
			}
			if pic, kind := flacBlock(b); pic != nil {
				if kind == 3 {
					return pic
				}
				if first == nil {
					first = pic
				}
			}
		} else if _, err := f.Seek(n, io.SeekCurrent); err != nil {
			return first
		}
		if last {
			break
		}
	}
	return first
}

// flacBlock reads a PICTURE block: its picture type and data.
func flacBlock(b []byte) ([]byte, uint32) {
	r := bytes.NewReader(b)
	var kind, l uint32
	read := func(v *uint32) bool { return binary.Read(r, binary.BigEndian, v) == nil }
	if !read(&kind) || !read(&l) || r.Len() < int(l) {
		return nil, 0
	}
	r.Seek(int64(l), io.SeekCurrent) // MIME type
	if !read(&l) || r.Len() < int(l) {
		return nil, 0
	}
	r.Seek(int64(l)+16, io.SeekCurrent) // description, width, height, depth, colours
	if !read(&l) || r.Len() < int(l) {
		return nil, 0
	}
	pic := make([]byte, l)
	r.Read(pic)
	return pic, kind
}

func syncsafe(b []byte) int64 {
	return int64(b[0]&127)<<21 | int64(b[1]&127)<<14 | int64(b[2]&127)<<7 | int64(b[3]&127)
}

// id3Picture finds an APIC frame (v2.3, v2.4) in the tag whose 10-byte header is head.
func id3Picture(f *os.File, head []byte) []byte {
	v := head[3]
	size := syncsafe(head[6:10])
	if (v != 3 && v != 4) || size > 16<<20 {
		return nil
	}
	tag := make([]byte, size)
	if _, err := io.ReadFull(f, tag); err != nil {
		return nil
	}
	var first []byte
	for at := 0; at+10 <= len(tag) && tag[at] != 0; {
		id := string(tag[at : at+4])
		var n int64
		if v == 4 {
			n = syncsafe(tag[at+4 : at+8])
		} else {
			n = int64(binary.BigEndian.Uint32(tag[at+4 : at+8]))
		}
		body := at + 10
		if n < 0 || int64(body)+n > int64(len(tag)) {
			break
		}
		if id == "APIC" {
			if pic, kind := apic(tag[body : body+int(n)]); pic != nil {
				if kind == 3 {
					return pic
				}
				if first == nil {
					first = pic
				}
			}
		}
		at = body + int(n)
	}
	return first
}

// apic reads an APIC frame's body: encoding, MIME type, picture type, description, data.
func apic(b []byte) ([]byte, byte) {
	if len(b) < 4 {
		return nil, 0
	}
	enc := b[0]
	i := bytes.IndexByte(b[1:], 0)
	if i < 0 || 1+i+2 > len(b) {
		return nil, 0
	}
	i += 2
	kind := b[i]
	i++
	// the description ends with a NUL (two, aligned, in UTF-16)
	if enc == 1 || enc == 2 {
		for i+1 < len(b) && !(b[i] == 0 && b[i+1] == 0) {
			i += 2
		}
		i += 2
	} else {
		for i < len(b) && b[i] != 0 {
			i++
		}
		i++
	}
	if i >= len(b) {
		return nil, 0
	}
	return b[i:], kind
}
