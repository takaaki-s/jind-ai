package remote

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"strings"
	"testing"
)

func TestFrameRoundTrip(t *testing.T) {
	want := struct {
		Name string `json:"name"`
	}{Name: "remote"}
	var framed bytes.Buffer
	if err := WriteFrame(&framed, want); err != nil {
		t.Fatalf("WriteFrame: %v", err)
	}
	var got struct {
		Name string `json:"name"`
	}
	if err := ReadFrame(&framed, &got); err != nil {
		t.Fatalf("ReadFrame: %v", err)
	}
	if got != want {
		t.Fatalf("round trip = %+v, want %+v", got, want)
	}
}

func TestWriteFrameRejectsOversize(t *testing.T) {
	var framed bytes.Buffer
	err := WriteFrame(&framed, strings.Repeat("x", MaxFrameBytes))
	if !errors.Is(err, ErrFrameTooLarge) || framed.Len() != 0 {
		t.Fatalf("WriteFrame = %v, bytes = %d", err, framed.Len())
	}
}

func TestReadFrameRejectsOversizeBeforeBody(t *testing.T) {
	var framed bytes.Buffer
	if err := binary.Write(&framed, binary.BigEndian, uint32(MaxFrameBytes+1)); err != nil {
		t.Fatal(err)
	}
	var got any
	if err := ReadFrame(&framed, &got); !errors.Is(err, ErrFrameTooLarge) {
		t.Fatalf("ReadFrame = %v", err)
	}
}

func TestReadFrameRejectsTruncatedAndMalformedFrames(t *testing.T) {
	t.Run("truncated", func(t *testing.T) {
		var framed bytes.Buffer
		_ = binary.Write(&framed, binary.BigEndian, uint32(5))
		framed.WriteString("{}")
		var got any
		if err := ReadFrame(&framed, &got); !errors.Is(err, io.ErrUnexpectedEOF) {
			t.Fatalf("ReadFrame = %v", err)
		}
	})
	t.Run("malformed JSON", func(t *testing.T) {
		var framed bytes.Buffer
		_ = binary.Write(&framed, binary.BigEndian, uint32(1))
		framed.WriteByte('{')
		var got any
		if err := ReadFrame(&framed, &got); !errors.Is(err, ErrInvalidFrame) {
			t.Fatalf("ReadFrame = %v", err)
		}
	})
}
