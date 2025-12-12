package protocol

import (
	"bytes"
	"encoding/json"
	"testing"
	"time"
)

func TestEncodeDecodeRoundTrip(t *testing.T) {
	buf := new(bytes.Buffer)
	frame := &Frame{
		Type:      FrameData,
		SessionID: 42,
		StreamID:  7,
		Seq:       128,
		Payload:   []byte("hello world"),
	}
	if err := frame.Encode(buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Type != FrameData {
		t.Fatalf("type mismatch: %v", decoded.Type)
	}
	if string(decoded.Payload) != "hello world" {
		t.Fatalf("payload mismatch: %s", decoded.Payload)
	}
	if decoded.SessionID != 42 || decoded.StreamID != 7 || decoded.Seq != 128 {
		t.Fatalf("header mismatch: %+v", decoded)
	}
}

func TestDecodeAckFrame(t *testing.T) {
	buf := new(bytes.Buffer)
	frame := &Frame{
		Type:     FrameAck,
		Ack:      &AckPayload{AckSeq: 512, Credit: 4096},
		Seq:      33,
		StreamID: 9,
	}
	if err := frame.Encode(buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Ack == nil {
		t.Fatalf("expected ack payload")
	}
	if decoded.Ack.AckSeq != 512 || decoded.Ack.Credit != 4096 {
		t.Fatalf("unexpected ack payload: %+v", decoded.Ack)
	}
}

func TestDecodeControlFrame(t *testing.T) {
	payload := &ControlPayload{
		Type:      ControlStreamOpen,
		SessionID: 1,
		StreamID:  2,
		Window:    65535,
		Metadata:  map[string]any{"addr": "example.com:80"},
	}
	buf := new(bytes.Buffer)
	frame := &Frame{Type: FrameControl, Control: payload}
	if err := frame.Encode(buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Control == nil {
		t.Fatalf("expected control payload")
	}
	if decoded.Control.Type != ControlStreamOpen {
		t.Fatalf("unexpected control type: %v", decoded.Control.Type)
	}
	if decoded.Control.Metadata["addr"] != "example.com:80" {
		t.Fatalf("metadata mismatch: %v", decoded.Control.Metadata)
	}
}

func TestDecodeHeartbeat(t *testing.T) {
	buf := new(bytes.Buffer)
	ts := time.Now().UnixNano()
	frame := &Frame{Type: FrameHeartbeat, Heartbeat: &HeartbeatPayload{UnixNanos: ts}}
	if err := frame.Encode(buf); err != nil {
		t.Fatalf("encode: %v", err)
	}
	decoded, err := Decode(bytes.NewReader(buf.Bytes()))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if decoded.Heartbeat == nil {
		t.Fatalf("expected heartbeat payload")
	}
	if decoded.Heartbeat.UnixNanos != ts {
		t.Fatalf("timestamp mismatch: %d != %d", decoded.Heartbeat.UnixNanos, ts)
	}
}

func TestControlPayloadJSONRoundTrip(t *testing.T) {
	original := ControlPayload{
		Type:      ControlStreamOpen,
		SessionID: 55,
		StreamID:  66,
		Window:    777,
		Metadata:  map[string]any{"host": "localhost", "port": json.Number("8080")},
	}
	b, err := json.Marshal(original)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var decoded ControlPayload
	if err := json.Unmarshal(b, &decoded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if decoded.Type != original.Type || decoded.SessionID != original.SessionID || decoded.StreamID != original.StreamID {
		t.Fatalf("payload mismatch: %+v", decoded)
	}
}
