package main

// Minimal RFC 6455 server side: enough for the web driver, which sends and receives
// short text frames. Stdlib only so the bridge stays a single static binary.

import (
	"bufio"
	"crypto/sha1"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
)

const wsGUID = "258EAFA5-E914-47DA-95CA-C5AB0DC85B11"

const (
	opCont   = 0x0
	opText   = 0x1
	opBinary = 0x2
	opClose  = 0x8
	opPing   = 0x9
	opPong   = 0xA

	maxFrame = 1 << 20
)

type wsConn struct {
	conn net.Conn
	rd   *bufio.Reader
	mu   sync.Mutex // serializes writes
}

// upgrade performs the WebSocket handshake. originOK decides whether the browser Origin
// header is acceptable; a page on another site must not be able to drive the mouse.
func upgrade(w http.ResponseWriter, r *http.Request, originOK func(string) bool) (*wsConn, error) {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") ||
		!strings.Contains(strings.ToLower(r.Header.Get("Connection")), "upgrade") {
		http.Error(w, "furycube-g3-linux-bridge: websocket only", http.StatusUpgradeRequired)
		return nil, errors.New("not a websocket request")
	}
	if o := r.Header.Get("Origin"); o != "" && !originOK(o) {
		http.Error(w, "origin not allowed", http.StatusForbidden)
		return nil, errors.New("origin rejected: " + o)
	}
	key := r.Header.Get("Sec-WebSocket-Key")
	if key == "" || r.Header.Get("Sec-WebSocket-Version") != "13" {
		http.Error(w, "bad websocket handshake", http.StatusBadRequest)
		return nil, errors.New("bad handshake")
	}
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "hijack unsupported", http.StatusInternalServerError)
		return nil, errors.New("hijack unsupported")
	}
	conn, rw, err := hj.Hijack()
	if err != nil {
		return nil, err
	}
	sum := sha1.Sum([]byte(key + wsGUID))
	resp := "HTTP/1.1 101 Switching Protocols\r\n" +
		"Upgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Accept: " + base64.StdEncoding.EncodeToString(sum[:]) + "\r\n\r\n"
	if _, err := rw.WriteString(resp); err != nil {
		conn.Close()
		return nil, err
	}
	if err := rw.Flush(); err != nil {
		conn.Close()
		return nil, err
	}
	return &wsConn{conn: conn, rd: rw.Reader}, nil
}

// readMessage returns the next complete text/binary message, answering pings on the way.
func (c *wsConn) readMessage() ([]byte, error) {
	var msg []byte
	for {
		op, fin, payload, err := c.readFrame()
		if err != nil {
			return nil, err
		}
		switch op {
		case opPing:
			if err := c.writeFrame(opPong, payload); err != nil {
				return nil, err
			}
		case opPong:
		case opClose:
			c.writeFrame(opClose, nil)
			return nil, io.EOF
		case opText, opBinary, opCont:
			msg = append(msg, payload...)
			if len(msg) > maxFrame {
				return nil, errors.New("message too large")
			}
			if fin {
				return msg, nil
			}
		default:
			return nil, errors.New("unknown opcode")
		}
	}
}

func (c *wsConn) readFrame() (op byte, fin bool, payload []byte, err error) {
	var h [2]byte
	if _, err = io.ReadFull(c.rd, h[:]); err != nil {
		return
	}
	fin = h[0]&0x80 != 0
	op = h[0] & 0x0F
	masked := h[1]&0x80 != 0
	n := uint64(h[1] & 0x7F)
	switch n {
	case 126:
		var b [2]byte
		if _, err = io.ReadFull(c.rd, b[:]); err != nil {
			return
		}
		n = uint64(binary.BigEndian.Uint16(b[:]))
	case 127:
		var b [8]byte
		if _, err = io.ReadFull(c.rd, b[:]); err != nil {
			return
		}
		n = binary.BigEndian.Uint64(b[:])
	}
	if n > maxFrame {
		err = errors.New("frame too large")
		return
	}
	if !masked { // clients must mask (RFC 6455 5.1)
		err = errors.New("unmasked client frame")
		return
	}
	var mask [4]byte
	if _, err = io.ReadFull(c.rd, mask[:]); err != nil {
		return
	}
	payload = make([]byte, n)
	if _, err = io.ReadFull(c.rd, payload); err != nil {
		return
	}
	for i := range payload {
		payload[i] ^= mask[i%4]
	}
	return
}

func (c *wsConn) writeFrame(op byte, payload []byte) error {
	hdr := []byte{0x80 | op}
	switch n := len(payload); {
	case n < 126:
		hdr = append(hdr, byte(n))
	case n <= 0xFFFF:
		hdr = append(hdr, 126, byte(n>>8), byte(n))
	default:
		hdr = append(hdr, 127)
		hdr = binary.BigEndian.AppendUint64(hdr, uint64(n))
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if _, err := c.conn.Write(append(hdr, payload...)); err != nil {
		return err
	}
	return nil
}

func (c *wsConn) writeText(s string) error { return c.writeFrame(opText, []byte(s)) }

func (c *wsConn) close() { c.conn.Close() }
