package remote

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

var (
	ErrFrameTooLarge = errors.New("remote frame exceeds 1 MiB")
	ErrInvalidFrame  = errors.New("invalid remote frame")
)

func WriteFrame(w io.Writer, value any) error {
	payload, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(payload) > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	var prefix [4]byte
	binary.BigEndian.PutUint32(prefix[:], uint32(len(payload)))
	if _, err := w.Write(prefix[:]); err != nil {
		return err
	}
	_, err = w.Write(payload)
	return err
}

func ReadFrame(r io.Reader, value any) error {
	var prefix [4]byte
	if _, err := io.ReadFull(r, prefix[:]); err != nil {
		return err
	}
	size := binary.BigEndian.Uint32(prefix[:])
	if size > MaxFrameBytes {
		return ErrFrameTooLarge
	}
	payload := make([]byte, size)
	if _, err := io.ReadFull(r, payload); err != nil {
		return err
	}
	if err := json.Unmarshal(payload, value); err != nil {
		return fmt.Errorf("%w: %v", ErrInvalidFrame, err)
	}
	return nil
}
