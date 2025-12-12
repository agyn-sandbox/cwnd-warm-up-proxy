package socks

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"net"
)

var (
	errVersionMismatch = errors.New("socks: unsupported version")
	errUnsupportedCMD  = errors.New("socks: unsupported command")
)

// Handshake performs a SOCKS5 CONNECT handshake and returns the requested
// destination address in host:port form.
func Handshake(conn net.Conn) (string, error) {
	if err := negotiate(conn); err != nil {
		return "", err
	}
	addr, err := readRequest(conn)
	if err != nil {
		return "", err
	}
	if err := sendSuccess(conn); err != nil {
		return "", err
	}
	return addr, nil
}

func negotiate(rw io.ReadWriter) error {
	buf := make([]byte, 2)
	if _, err := io.ReadFull(rw, buf); err != nil {
		return err
	}
	if buf[0] != 0x05 {
		return errVersionMismatch
	}
	nMethods := int(buf[1])
	methods := make([]byte, nMethods)
	if _, err := io.ReadFull(rw, methods); err != nil {
		return err
	}
	// We only support "no authentication required" (0x00)
	resp := []byte{0x05, 0xFF}
	for _, m := range methods {
		if m == 0x00 {
			resp[1] = 0x00
			break
		}
	}
	if resp[1] == 0xFF {
		return errors.New("socks: no acceptable authentication method")
	}
	if _, err := rw.Write(resp); err != nil {
		return err
	}
	return nil
}

func readRequest(r io.Reader) (string, error) {
	head := make([]byte, 4)
	if _, err := io.ReadFull(r, head); err != nil {
		return "", err
	}
	if head[0] != 0x05 {
		return "", errVersionMismatch
	}
	if head[1] != 0x01 {
		return "", errUnsupportedCMD
	}
	addrType := head[3]
	var host string
	switch addrType {
	case 0x01: // IPv4
		addr := make([]byte, 4)
		if _, err := io.ReadFull(r, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	case 0x03: // Domain
		var ln [1]byte
		if _, err := io.ReadFull(r, ln[:]); err != nil {
			return "", err
		}
		addr := make([]byte, ln[0])
		if _, err := io.ReadFull(r, addr); err != nil {
			return "", err
		}
		host = string(addr)
	case 0x04: // IPv6
		addr := make([]byte, 16)
		if _, err := io.ReadFull(r, addr); err != nil {
			return "", err
		}
		host = net.IP(addr).String()
	default:
		return "", fmt.Errorf("socks: unsupported address type %d", addrType)
	}
	var portBuf [2]byte
	if _, err := io.ReadFull(r, portBuf[:]); err != nil {
		return "", err
	}
	port := binary.BigEndian.Uint16(portBuf[:])
	return net.JoinHostPort(host, fmt.Sprintf("%d", port)), nil
}

func sendSuccess(w io.Writer) error {
	resp := []byte{0x05, 0x00, 0x00, 0x01}
	resp = append(resp, []byte{0, 0, 0, 0}...)
	resp = append(resp, []byte{0, 0}...)
	_, err := w.Write(resp)
	return err
}
