package protocol

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Constants describing the protocol framing format. The header is fixed width
// and uses network byte order for multi-byte integers so that the data is
// stable across different architectures.

const (
	// Version is the current frame version supported by this implementation.
	Version uint8 = 1

	// HeaderSize is the number of bytes in every frame header.
	HeaderSize = 24

	// MaxPayloadSize is an upper bound used to protect against unbounded
	// allocations when decoding frames. The scheduler keeps payloads under this
	// size, so attempts to decode larger payloads are treated as protocol errors.
	MaxPayloadSize = 1 << 20 // 1 MiB
)

// FrameType represents the 1-byte type field in the frame header.
type FrameType uint8

const (
	FrameData      FrameType = 0x01
	FrameAck       FrameType = 0x02
	FrameControl   FrameType = 0x03
	FrameHeartbeat FrameType = 0x04
)

// ControlType identifies the semantic meaning of a control frame payload.
type ControlType uint8

const (
	ControlSessionInit   ControlType = 0x01
	ControlSessionAccept ControlType = 0x02
	ControlSessionJoin   ControlType = 0x03
	ControlStreamOpen    ControlType = 0x10
	ControlStreamAccept  ControlType = 0x11
	ControlStreamClose   ControlType = 0x12
	ControlStreamReset   ControlType = 0x13
)

// Frame represents a decoded protocol frame. Only the fields relevant to the
// concrete frame type are populated.
type Frame struct {
	Version   uint8
	Type      FrameType
	Flags     uint8
	SessionID uint32
	StreamID  uint32
	Seq       uint64
	Payload   []byte

	Ack       *AckPayload
	Control   *ControlPayload
	Heartbeat *HeartbeatPayload
}

// AckPayload describes the payload carried in ACK frames.
type AckPayload struct {
	AckSeq uint64 `json:"ack_seq"`
	Credit uint32 `json:"credit"`
}

// ControlPayload is JSON encoded inside control frames to keep the wire format
// extensible without complicating the prototype implementation.
type ControlPayload struct {
	Type      ControlType     `json:"type"`
	SessionID uint32          `json:"session_id,omitempty"`
	StreamID  uint32          `json:"stream_id,omitempty"`
	Window    uint32          `json:"window,omitempty"`
	Metadata  map[string]any  `json:"metadata,omitempty"`
	Data      map[string]any  `json:"data,omitempty"`
	Raw       json.RawMessage `json:"raw,omitempty"`
}

// HeartbeatPayload holds the timestamp embedded in heartbeat frames.
type HeartbeatPayload struct {
	UnixNanos int64 `json:"unix_nanos"`
}

var (
	errUnsupportedVersion = errors.New("overlay/protocol: unsupported frame version")
	errPayloadTooLarge    = errors.New("overlay/protocol: payload exceeds limit")
	errInvalidFrameType   = errors.New("overlay/protocol: invalid frame type")
	errPayloadMismatch    = errors.New("overlay/protocol: payload missing for frame type")
)

// Encode writes the frame into the provided writer.
func (f *Frame) Encode(w io.Writer) error {
	if f.Version == 0 {
		f.Version = Version
	}
	payload, err := f.payloadBytes()
	if err != nil {
		return err
	}
	if len(payload) > MaxPayloadSize {
		return errPayloadTooLarge
	}
	header := make([]byte, HeaderSize)
	header[0] = f.Version
	header[1] = byte(f.Type)
	header[2] = f.Flags
	// header[3] reserved
	binary.BigEndian.PutUint32(header[4:8], f.SessionID)
	binary.BigEndian.PutUint32(header[8:12], f.StreamID)
	binary.BigEndian.PutUint64(header[12:20], f.Seq)
	binary.BigEndian.PutUint32(header[20:24], uint32(len(payload)))
	if _, err := w.Write(header); err != nil {
		return err
	}
	if len(payload) == 0 {
		return nil
	}
	_, err = w.Write(payload)
	return err
}

// payloadBytes materialises the frame payload for writing.
func (f *Frame) payloadBytes() ([]byte, error) {
	if len(f.Payload) > 0 {
		return f.Payload, nil
	}
	switch f.Type {
	case FrameData:
		return f.Payload, nil
	case FrameAck:
		if f.Ack == nil {
			return nil, errPayloadMismatch
		}
		buf := make([]byte, 12)
		binary.BigEndian.PutUint64(buf[0:8], f.Ack.AckSeq)
		binary.BigEndian.PutUint32(buf[8:12], f.Ack.Credit)
		return buf, nil
	case FrameControl:
		if f.Control == nil {
			return nil, errPayloadMismatch
		}
		return json.Marshal(f.Control)
	case FrameHeartbeat:
		if f.Heartbeat == nil {
			return nil, errPayloadMismatch
		}
		buf := make([]byte, 8)
		binary.BigEndian.PutUint64(buf, uint64(f.Heartbeat.UnixNanos))
		return buf, nil
	default:
		return nil, errInvalidFrameType
	}
}

// Decode reads a single frame from r.
func Decode(r io.Reader) (*Frame, error) {
	header := make([]byte, HeaderSize)
	if _, err := io.ReadFull(r, header); err != nil {
		return nil, err
	}
	frame := &Frame{
		Version:   header[0],
		Type:      FrameType(header[1]),
		Flags:     header[2],
		SessionID: binary.BigEndian.Uint32(header[4:8]),
		StreamID:  binary.BigEndian.Uint32(header[8:12]),
		Seq:       binary.BigEndian.Uint64(header[12:20]),
	}
	if frame.Version != Version {
		return nil, fmt.Errorf("%w: %d", errUnsupportedVersion, frame.Version)
	}
	length := binary.BigEndian.Uint32(header[20:24])
	if length > MaxPayloadSize {
		return nil, errPayloadTooLarge
	}
	if length == 0 {
		return frame, nil
	}
	buf := make([]byte, length)
	if _, err := io.ReadFull(r, buf); err != nil {
		return nil, err
	}
	switch frame.Type {
	case FrameData:
		frame.Payload = buf
	case FrameAck:
		if len(buf) != 12 {
			return nil, fmt.Errorf("overlay/protocol: invalid ack payload length %d", len(buf))
		}
		frame.Ack = &AckPayload{
			AckSeq: binary.BigEndian.Uint64(buf[0:8]),
			Credit: binary.BigEndian.Uint32(buf[8:12]),
		}
	case FrameControl:
		var payload ControlPayload
		if err := json.Unmarshal(buf, &payload); err != nil {
			return nil, fmt.Errorf("overlay/protocol: decode control: %w", err)
		}
		frame.Control = &payload
	case FrameHeartbeat:
		if len(buf) != 8 {
			return nil, fmt.Errorf("overlay/protocol: invalid heartbeat payload length %d", len(buf))
		}
		frame.Heartbeat = &HeartbeatPayload{UnixNanos: int64(binary.BigEndian.Uint64(buf))}
	default:
		return nil, errInvalidFrameType
	}
	return frame, nil
}

// ReadFrame is an alias for Decode to provide an easy migration path when the
// prototype grows additional helpers.
func ReadFrame(r io.Reader) (*Frame, error) {
	return Decode(r)
}
