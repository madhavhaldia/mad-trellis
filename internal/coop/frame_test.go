package coop

import (
	"bytes"
	"crypto/rand"
	"errors"
	"io"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	cases := []struct {
		name string
		id   uint32
		typ  FrameType
		size int
	}{
		{"open-no-payload", 1, FrameOpen, 0},
		{"ready-zero-stream", 0, FrameReady, 0},
		{"close-no-payload", 7, FrameClose, 0},
		{"data-1b", 2, FrameData, 1},
		{"data-small", 42, FrameData, 1500},
		{"data-64k", 99, FrameData, 65536},
		{"data-max", 100, FrameData, MaxPayload},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := make([]byte, tc.size)
			_, _ = rand.Read(payload)
			var buf bytes.Buffer
			if err := WriteFrame(&buf, tc.id, tc.typ, payload); err != nil {
				t.Fatalf("WriteFrame: %v", err)
			}
			got, err := ReadFrame(&buf)
			if err != nil {
				t.Fatalf("ReadFrame: %v", err)
			}
			if got.StreamID != tc.id || got.Type != tc.typ {
				t.Fatalf("header mismatch: got (%d,%d) want (%d,%d)", got.StreamID, got.Type, tc.id, tc.typ)
			}
			if !bytes.Equal(got.Payload, payload) {
				t.Fatalf("payload mismatch for %s (len got %d want %d)", tc.name, len(got.Payload), len(payload))
			}
			if buf.Len() != 0 {
				t.Fatalf("trailing bytes after frame: %d", buf.Len())
			}
		})
	}
}

// TestMultipleFramesStreamInOrder proves several frames concatenated on one
// stream decode back in order (the relay/pump read a continuous frame stream).
func TestMultipleFramesStreamInOrder(t *testing.T) {
	var buf bytes.Buffer
	want := []Frame{
		{StreamID: 1, Type: FrameOpen, Payload: nil},
		{StreamID: 1, Type: FrameData, Payload: []byte("hello")},
		{StreamID: 2, Type: FrameOpen, Payload: nil},
		{StreamID: 2, Type: FrameData, Payload: []byte("world!!")},
		{StreamID: 1, Type: FrameClose, Payload: nil},
	}
	for _, f := range want {
		if err := WriteFrame(&buf, f.StreamID, f.Type, f.Payload); err != nil {
			t.Fatalf("write: %v", err)
		}
	}
	for i, w := range want {
		got, err := ReadFrame(&buf)
		if err != nil {
			t.Fatalf("read %d: %v", i, err)
		}
		if got.StreamID != w.StreamID || got.Type != w.Type || !bytes.Equal(got.Payload, []byte(string(w.Payload))) {
			t.Fatalf("frame %d mismatch: got %+v want %+v", i, got, w)
		}
	}
}

func TestWriteFrameRejectsOversizePayload(t *testing.T) {
	var buf bytes.Buffer
	err := WriteFrame(&buf, 1, FrameData, make([]byte, MaxPayload+1))
	if err == nil {
		t.Fatal("expected oversize payload to be rejected")
	}
	if buf.Len() != 0 {
		t.Fatal("nothing should be written when the payload is rejected")
	}
}

func TestReadFrameRejectsOversizeLength(t *testing.T) {
	// Hand-craft a header claiming MaxPayload+1 bytes — ReadFrame must reject it
	// WITHOUT trying to allocate/read the body (hostile-length guard).
	var buf bytes.Buffer
	hdr := make([]byte, headerLen)
	hdr[4] = byte(FrameData)
	// length field = MaxPayload+1
	n := uint32(MaxPayload + 1)
	hdr[5] = byte(n >> 24)
	hdr[6] = byte(n >> 16)
	hdr[7] = byte(n >> 8)
	hdr[8] = byte(n)
	buf.Write(hdr)
	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("expected oversize length to be rejected")
	}
}

func TestReadFrameRejectsUnknownType(t *testing.T) {
	var buf bytes.Buffer
	hdr := make([]byte, headerLen)
	hdr[4] = 99 // not a known type
	buf.Write(hdr)
	if _, err := ReadFrame(&buf); err == nil {
		t.Fatal("expected unknown frame type to be rejected (stream desync)")
	}
}

func TestReadFrameEOFAtBoundaryIsClean(t *testing.T) {
	var buf bytes.Buffer
	if _, err := ReadFrame(&buf); !errors.Is(err, io.EOF) {
		t.Fatalf("clean EOF at a frame boundary should be io.EOF, got %v", err)
	}
}

func TestReadFrameTruncatedIsUnexpectedEOF(t *testing.T) {
	var buf bytes.Buffer
	// a full header promising 10 bytes, but only 3 present
	if err := writeHeaderOnly(&buf, 1, FrameData, 10); err != nil {
		t.Fatal(err)
	}
	buf.Write([]byte("abc"))
	if _, err := ReadFrame(&buf); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("truncated payload should be ErrUnexpectedEOF, got %v", err)
	}
}

func writeHeaderOnly(buf *bytes.Buffer, id uint32, t FrameType, n uint32) error {
	hdr := make([]byte, headerLen)
	hdr[0] = byte(id >> 24)
	hdr[1] = byte(id >> 16)
	hdr[2] = byte(id >> 8)
	hdr[3] = byte(id)
	hdr[4] = byte(t)
	hdr[5] = byte(n >> 24)
	hdr[6] = byte(n >> 16)
	hdr[7] = byte(n >> 8)
	hdr[8] = byte(n)
	_, err := buf.Write(hdr)
	return err
}
