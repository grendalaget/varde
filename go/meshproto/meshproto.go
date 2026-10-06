// Package meshproto implements the wire codecs shared by the mesh node, the
// relay server and tests:
//
//   - varint-length-prefixed protobuf frames on QUIC streams (frames.proto)
//   - relay datagrams (relay.proto): single type byte < 0x40 payloads
//   - UDP-flow datagrams on a QUIC connection: [uvarint flow_id][payload]
//   - the relay REGISTER signing string
//
// The CP-signed relay token itself lives in go/relaytoken.
package meshproto

import (
	"crypto/ed25519"
	"encoding/binary"
	"errors"
	"fmt"
	"io"

	"google.golang.org/protobuf/proto"
)

// MaxFrameSize bounds a single stream frame (StreamOpen/StreamAccept/
// ControlFrame) to keep decoders from allocating on a hostile length prefix.
const MaxFrameSize = 1 << 20

// MaxDatagram is the largest relay/UDP datagram payload we accept.
const MaxDatagram = 1 << 17

// WriteFrame marshals msg and writes it length-prefixed (uvarint) to w.
func WriteFrame(w io.Writer, msg proto.Message) error {
	b, err := proto.Marshal(msg)
	if err != nil {
		return err
	}
	var hdr [binary.MaxVarintLen64]byte
	n := binary.PutUvarint(hdr[:], uint64(len(b)))
	if _, err := w.Write(hdr[:n]); err != nil {
		return err
	}
	_, err = w.Write(b)
	return err
}

// ReadFrame reads one length-prefixed protobuf message from r.
func ReadFrame(r io.Reader, msg proto.Message) error {
	var n uint64
	var shift uint
	for i := 0; i < binary.MaxVarintLen64; i++ {
		var b [1]byte
		if _, err := io.ReadFull(r, b[:]); err != nil {
			return err
		}
		n |= uint64(b[0]&0x7f) << shift
		if b[0]&0x80 == 0 {
			break
		}
		shift += 7
	}
	if n > MaxFrameSize {
		return fmt.Errorf("meshproto: frame too large (%d)", n)
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(r, buf); err != nil {
		return err
	}
	return proto.Unmarshal(buf, msg)
}

// ---- relay datagrams (proto/mesh/v1/relay.proto header) ----

const (
	DgramRegister   byte = 0x01
	DgramRegistered byte = 0x02
	DgramData       byte = 0x03
	DgramPing       byte = 0x04
	DgramPong       byte = 0x05
	DgramError      byte = 0x06
)

// EncodeProtoDgram returns [type byte][protobuf] for REGISTER/REGISTERED/ERROR.
func EncodeProtoDgram(typ byte, msg proto.Message) ([]byte, error) {
	b, err := proto.Marshal(msg)
	if err != nil {
		return nil, err
	}
	out := make([]byte, 0, 1+len(b))
	out = append(out, typ)
	out = append(out, b...)
	return out, nil
}

// DecodeProtoDgram splits a datagram into its type byte and unmarshals the rest.
func DecodeProtoDgram(dgram []byte, msg proto.Message) (byte, error) {
	if len(dgram) < 1 || len(dgram) > MaxDatagram {
		return 0, errors.New("meshproto: bad datagram size")
	}
	return dgram[0], proto.Unmarshal(dgram[1:], msg)
}

// EncodeData builds a node->relay DATA datagram: 0x03 [u8 len][dst][payload].
func EncodeData(dstNodeID, payload string) []byte {
	return EncodeDataBytes(dstNodeID, []byte(payload))
}

// EncodeDataBytes is EncodeData for arbitrary payloads.
func EncodeDataBytes(dstNodeID string, payload []byte) []byte {
	out := make([]byte, 0, 2+len(dstNodeID)+len(payload))
	out = append(out, DgramData, byte(len(dstNodeID)))
	out = append(out, dstNodeID...)
	out = append(out, payload...)
	return out
}

// DecodeData parses a DATA datagram (either direction: the id is the dst when
// sent to the relay, the src when received from it).
func DecodeData(dgram []byte) (nodeID string, payload []byte, err error) {
	if len(dgram) < 2 || dgram[0] != DgramData {
		return "", nil, errors.New("meshproto: not a DATA datagram")
	}
	l := int(dgram[1])
	if len(dgram) < 2+l {
		return "", nil, errors.New("meshproto: truncated DATA datagram")
	}
	return string(dgram[2 : 2+l]), dgram[2+l:], nil
}

// EncodePing/EncodePong build the 8-byte nonce datagrams.
func EncodePing(nonce uint64) []byte { return encodeNonce(DgramPing, nonce) }
func EncodePong(nonce uint64) []byte { return encodeNonce(DgramPong, nonce) }

func encodeNonce(typ byte, nonce uint64) []byte {
	out := make([]byte, 9)
	out[0] = typ
	binary.BigEndian.PutUint64(out[1:], nonce)
	return out
}

// DecodeNonce decodes a PING/PONG datagram and returns its type and nonce.
func DecodeNonce(dgram []byte) (typ byte, nonce uint64, err error) {
	if len(dgram) != 9 || (dgram[0] != DgramPing && dgram[0] != DgramPong) {
		return 0, 0, errors.New("meshproto: bad nonce datagram")
	}
	return dgram[0], binary.BigEndian.Uint64(dgram[1:]), nil
}

// ---- UDP-flow datagrams on a peer QUIC connection ----

// EncodeFlowDatagram returns [uvarint flow_id][payload].
func EncodeFlowDatagram(flowID uint64, payload []byte) []byte {
	out := make([]byte, 0, binary.MaxVarintLen64+len(payload))
	out = binary.AppendUvarint(out, flowID)
	return append(out, payload...)
}

// DecodeFlowDatagram splits [uvarint flow_id][payload].
func DecodeFlowDatagram(dgram []byte) (flowID uint64, payload []byte, err error) {
	id, n := binary.Uvarint(dgram)
	if n <= 0 {
		return 0, nil, errors.New("meshproto: bad flow datagram")
	}
	return id, dgram[n:], nil
}

// ---- relay REGISTER signing ----

// RegisterSigningString is the canonical string the node signs when
// registering with a relay:
// "varde-relay-register|<relay_id>|<node_id>|<timestamp_unix_ms>".
func RegisterSigningString(relayID, nodeID string, timestampUnixMs int64) string {
	return fmt.Sprintf("varde-relay-register|%s|%s|%d", relayID, nodeID, timestampUnixMs)
}

// SignRegister signs the register string with the node key.
func SignRegister(priv ed25519.PrivateKey, relayID, nodeID string, timestampUnixMs int64) []byte {
	return ed25519.Sign(priv, []byte(RegisterSigningString(relayID, nodeID, timestampUnixMs)))
}

// VerifyRegister checks the node's register signature.
func VerifyRegister(pub ed25519.PublicKey, relayID, nodeID string, timestampUnixMs int64, sig []byte) bool {
	return ed25519.Verify(pub, []byte(RegisterSigningString(relayID, nodeID, timestampUnixMs)), sig)
}
