// Copyright (c) Tailscale Inc & AUTHORS
// SPDX-License-Identifier: BSD-3-Clause

package guestnet

import (
	"errors"
	"io"
	"os"
	"sync"
)

// FrameLink carries raw Ethernet frames, one per call, between a guest NIC
// and a [Net].
type FrameLink interface {
	// ReadFrame reads one frame into buf and returns its length.
	ReadFrame(buf []byte) (int, error)
	// WriteFrame writes one frame.
	WriteFrame(frame []byte) error
	// Close closes the link and unblocks readers.
	Close() error
}

// FileLink is a FrameLink over a datagram socket held as an *os.File, such
// as one end of a SOCK_DGRAM socketpair whose other end is attached to a
// VZFileHandleNetworkDeviceAttachment. Each Read returns one datagram, which
// is one Ethernet frame.
type FileLink struct {
	f *os.File
}

// NewFileLink wraps a datagram socket. It takes ownership of f. The socket
// should have been non-blocking when f was made with os.NewFile; otherwise
// Close does not unblock a pending ReadFrame.
func NewFileLink(f *os.File) *FileLink { return &FileLink{f: f} }

// ReadFrame implements FrameLink.
func (l *FileLink) ReadFrame(buf []byte) (int, error) {
	for {
		n, err := l.f.Read(buf)
		if err != nil {
			return 0, err
		}
		if n == 0 {
			// Zero-length datagrams are not frames.
			continue
		}
		return n, nil
	}
}

// WriteFrame implements FrameLink.
func (l *FileLink) WriteFrame(frame []byte) error {
	_, err := l.f.Write(frame)
	return err
}

// Close implements FrameLink.
func (l *FileLink) Close() error { return l.f.Close() }

// MemLink is an in-memory FrameLink for tests. Create a connected pair with
// [NewMemLinkPair].
type MemLink struct {
	in   chan []byte
	out  chan []byte
	done chan struct{}
	once sync.Once
	peer *MemLink
}

// NewMemLinkPair returns two links where frames written to one are read from
// the other. Closing either end unblocks both.
func NewMemLinkPair() (a, b *MemLink) {
	ab := make(chan []byte, 1024)
	ba := make(chan []byte, 1024)
	done := make(chan struct{})
	a = &MemLink{in: ba, out: ab, done: done}
	b = &MemLink{in: ab, out: ba, done: done}
	a.peer, b.peer = b, a
	return a, b
}

// ReadFrame implements FrameLink.
func (l *MemLink) ReadFrame(buf []byte) (int, error) {
	select {
	case f := <-l.in:
		if len(f) > len(buf) {
			return 0, io.ErrShortBuffer
		}
		return copy(buf, f), nil
	case <-l.done:
		return 0, errors.New("memlink: closed")
	}
}

// WriteFrame implements FrameLink.
func (l *MemLink) WriteFrame(frame []byte) error {
	f := append([]byte(nil), frame...)
	select {
	case l.out <- f:
		return nil
	case <-l.done:
		return errors.New("memlink: closed")
	}
}

// Close implements FrameLink. It closes both ends of the pair.
func (l *MemLink) Close() error {
	l.once.Do(func() { close(l.done) })
	if l.peer != nil {
		l.peer.once.Do(func() {})
	}
	return nil
}
