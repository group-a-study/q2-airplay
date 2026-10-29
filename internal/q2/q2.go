// Package q2 holds what the card app shares with Q2 Pod's side (q2-pod's patch/addon.c): the status
// file it reads and CLOCK_MONOTONIC, the clock the payload and hciplayer's filter read.
package q2

import (
	"os"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unsafe"
)

// MonoNS is CLOCK_MONOTONIC in nanoseconds.
func MonoNS() int64 {
	var ts syscall.Timespec
	syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 1, uintptr(unsafe.Pointer(&ts)), 0)
	return int64(ts.Sec)*1e9 + int64(ts.Nsec)
}

// RawNS is CLOCK_MONOTONIC_RAW in nanoseconds: what NTP never steps or slews.
func RawNS() int64 {
	var ts syscall.Timespec
	syscall.Syscall(syscall.SYS_CLOCK_GETTIME, 4, uintptr(unsafe.Pointer(&ts)), 0)
	return int64(ts.Sec)*1e9 + int64(ts.Nsec)
}

// State is a key=value status file rewritten whole (a .tmp, renamed), so a reader never sees half of
// one. Values have newlines replaced, as the payload's field() reads a line.
type State struct {
	Path string
	mu   sync.Mutex
	last string
}

// Write replaces the file with kv, in key order, when it differs from the last one written.
func (s *State) Write(kv map[string]string) error {
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + strings.NewReplacer("\n", " ", "\r", " ").Replace(kv[k]) + "\n")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if b.String() == s.last {
		return nil
	}
	if err := WriteFile(s.Path, []byte(b.String())); err != nil {
		return err
	}
	s.last = b.String()
	return nil
}

// Remove deletes the file, as an app does when it exits.
func (s *State) Remove() {
	s.mu.Lock()
	defer s.mu.Unlock()
	os.Remove(s.Path)
	s.last = ""
}

// WriteFile writes data aside and renames it into place, synced first so a card write survives.
func WriteFile(path string, data []byte) error {
	tmp := path + ".tmp"
	f, err := os.Create(tmp)
	if err != nil {
		return err
	}
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = os.Rename(tmp, path)
	}
	if err != nil {
		os.Remove(tmp)
	}
	return err
}
