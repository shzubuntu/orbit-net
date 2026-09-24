package protocol

import (
	"encoding/binary"
	"errors"
	"io"
)

// ErrFrameTooBig 帧 payload 超过 MaxPayload。
var ErrFrameTooBig = errors.New("frame payload exceeds MaxPayload")

var errShortBuffer = errors.New("short payload buffer")

// WriteFrame 写一帧到 w。
func WriteFrame(w io.Writer, ftype byte, payload []byte) error {
	if len(payload) > MaxPayload {
		return ErrFrameTooBig
	}
	head := [3]byte{ftype, 0, 0}
	binary.BigEndian.PutUint16(head[1:], uint16(len(payload)))
	if _, err := w.Write(head[:]); err != nil {
		return err
	}
	_, err := w.Write(payload)
	return err
}

// ReadFrame 读一帧, payload 复用 buf(可传 nil)。
func ReadFrame(r io.Reader, buf []byte) (byte, []byte, error) {
	var head [3]byte
	if _, err := io.ReadFull(r, head[:]); err != nil {
		return 0, nil, err
	}
	n := int(binary.BigEndian.Uint16(head[1:]))
	if n > MaxPayload {
		return 0, nil, ErrFrameTooBig
	}
	if cap(buf) < n {
		buf = make([]byte, n)
	}
	p := buf[:n]
	if _, err := io.ReadFull(r, p); err != nil {
		return 0, nil, err
	}
	_ = errShortBuffer
	return head[0], p, nil
}
