package main

import (
	"bytes"
	"io"
	"testing"
	"testing/iotest"
)

func TestLastBytes(t *testing.T) {
	data := bytes.Repeat([]byte("abcdefghij"), 100_000) // 1 MB
	data = append(data, []byte("THE-FINAL-32-BYTES-OF-THE-STREAM")...)
	want := "THE-FINAL-32-BYTES-OF-THE-STREAM"

	// However the stream is chunked, the tail is the same.
	readers := map[string]io.Reader{
		"all at once":   bytes.NewReader(data),
		"one byte":      iotest.OneByteReader(bytes.NewReader(data)),
		"half reads":    iotest.HalfReader(bytes.NewReader(data)),
		"data with EOF": iotest.DataErrReader(bytes.NewReader(data)),
	}
	for name, r := range readers {
		got, err := lastBytes(r, 32)
		if err != nil || string(got) != want {
			t.Errorf("%s: got %q, %v", name, got, err)
		}
	}
	if _, err := lastBytes(bytes.NewReader([]byte("short")), 32); err == nil {
		t.Error("short stream accepted")
	}
	if _, err := lastBytes(iotest.TimeoutReader(bytes.NewReader(data)), 32); err == nil {
		t.Error("read error swallowed")
	}
}
