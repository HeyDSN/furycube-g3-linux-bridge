package main

import (
	"bufio"
	"bytes"
	"crypto/sha1"
	"encoding/base64"
	"encoding/hex"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"
)

// Report descriptor of the G3 dongle's vendor interface, read from the real device.
const g3Descriptor = "05010980a101850105011981298315002501950375018106950175058101c0050c0901a1018502150026800319002a8003751095018100c0050a0900a101850319a029a015002500750895048102c0050b0900a1018504190129019537b102850519022902950eb1028506190329039508b1028507190429049507b102850819052905953ab102850919062906953fb102850a190729079507b102850b190829089507b102850c190929099509b1028510190a290a9507b10285a0190b290b9507b1028522190c290c953fb102c0"

func TestFeatureSizes(t *testing.T) {
	desc, _ := hex.DecodeString(g3Descriptor)
	got := featureSizes(desc)
	want := map[byte]int{0x04: 56, 0x05: 15, 0x06: 9, 0x07: 8, 0x08: 59, 0x09: 64, 0x0A: 8, 0x0B: 8, 0x0C: 10, 0x10: 8, 0xA0: 8}
	for id, size := range want {
		if got[id] != size {
			t.Errorf("report %#x: got %d bytes, want %d", id, got[id], size)
		}
	}
}

func testHub(t *testing.T) (*hub, *[][]byte) {
	t.Helper()
	h := newHub(false)
	var sent [][]byte
	h.writeFn = func(_ *os.File, pkt []byte) error { sent = append(sent, pkt); return nil }
	h.file, _ = os.Open(os.DevNull)
	h.dev = &deviceInfo{conn: connWireless, sizes: map[byte]int{0x04: 56, 0x08: 59, 0x0C: 10, 0xA0: 8}}
	return h, &sent
}

func TestForwardPadsAndTruncates(t *testing.T) {
	h, sent := testHub(t)
	long := append([]byte{0x04, 0x38}, bytes.Repeat([]byte{0x11}, 70)...) // web driver sends 60+
	if !h.forward(g3DeviceID, long) {
		t.Fatal("forward failed")
	}
	if len((*sent)[0]) != 56 || (*sent)[0][0] != 0x04 {
		t.Errorf("DPI report not cut to 56 bytes: %d", len((*sent)[0]))
	}
	if !h.forward(g3DeviceID, []byte{0x0C, 0x0A, 0x01}) {
		t.Fatal("forward short failed")
	}
	if got := (*sent)[1]; len(got) != 10 || got[2] != 0x01 || got[9] != 0 {
		t.Errorf("short report not zero padded: % x", got)
	}
}

func TestForwardRefusals(t *testing.T) {
	h, sent := testHub(t)
	if h.forward(0x01, []byte{0x04, 0}) {
		t.Error("wrong device id accepted")
	}
	if h.forward(g3DeviceID, []byte{0xA0, 0}) {
		t.Error("calibration report 0xA0 accepted")
	}
	if h.forward(g3DeviceID, []byte{0x07, 0}) {
		t.Error("report with no size accepted")
	}
	h.file = nil
	if h.forward(g3DeviceID, []byte{0x04, 0}) {
		t.Error("write accepted with no mouse attached")
	}
	if len(*sent) != 0 {
		t.Errorf("nothing should have been written, got %d writes", len(*sent))
	}
}

// dial opens a raw websocket to the test server and returns helpers to talk to it.
func dial(t *testing.T, srv *httptest.Server, origin string) (net.Conn, *bufio.Reader, int) {
	t.Helper()
	conn, err := net.Dial("tcp", strings.TrimPrefix(srv.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	key := base64.StdEncoding.EncodeToString([]byte("0123456789abcdef"))
	req := "GET / HTTP/1.1\r\nHost: localhost\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n" +
		"Sec-WebSocket-Version: 13\r\nSec-WebSocket-Key: " + key + "\r\n"
	if origin != "" {
		req += "Origin: " + origin + "\r\n"
	}
	conn.Write([]byte(req + "\r\n"))
	rd := bufio.NewReader(conn)
	resp, err := http.ReadResponse(rd, nil)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode == 101 {
		sum := sha1.Sum([]byte(key + wsGUID))
		if resp.Header.Get("Sec-WebSocket-Accept") != base64.StdEncoding.EncodeToString(sum[:]) {
			t.Error("bad Sec-WebSocket-Accept")
		}
	}
	return conn, rd, resp.StatusCode
}

func sendText(conn net.Conn, s string) {
	mask := []byte{1, 2, 3, 4}
	frame := []byte{0x81, 0x80 | byte(len(s))}
	frame = append(frame, mask...)
	for i := 0; i < len(s); i++ {
		frame = append(frame, s[i]^mask[i%4])
	}
	conn.Write(frame)
}

func readText(t *testing.T, conn net.Conn, rd *bufio.Reader) string {
	t.Helper()
	conn.SetReadDeadline(time.Now().Add(2 * time.Second))
	hdr := make([]byte, 2)
	if _, err := rd.Read(hdr[:1]); err != nil {
		t.Fatal(err)
	}
	b, _ := rd.ReadByte()
	buf := make([]byte, b&0x7F)
	for n := 0; n < len(buf); {
		m, err := rd.Read(buf[n:])
		if err != nil {
			t.Fatal(err)
		}
		n += m
	}
	return string(buf)
}

func TestWebSocketSession(t *testing.T) {
	h, sent := testHub(t)
	h.status = []byte{connWireless, 0x01, g3DeviceID, 0x41, 0x01, 0x4E}
	srv := httptest.NewServer(h.serve(originPolicy("")))
	defer srv.Close()

	conn, rd, code := dial(t, srv, "https://www.bk-gmice.com")
	defer conn.Close()
	if code != 101 {
		t.Fatalf("handshake status %d", code)
	}
	if got := readText(t, conn, rd); got != helperVersion {
		t.Errorf("first frame %q, want version %q", got, helperVersion)
	}
	if got := readText(t, conn, rd); got != "81016641014e" {
		t.Errorf("presence frame %q", got)
	}
	sendText(conn, "81660438")
	if got := readText(t, conn, rd); got != "8166500000" {
		t.Errorf("ack %q, want 8166500000", got)
	}
	if len(*sent) != 1 || len((*sent)[0]) != 56 {
		t.Errorf("report not forwarded as 56 bytes: %v", *sent)
	}
	sendText(conn, "81660a0000") // 0x0A is not an allowed report
	if got := readText(t, conn, rd); got != "8166500100" {
		t.Errorf("nack %q, want 8166500100", got)
	}
}

func TestOriginPolicy(t *testing.T) {
	ok := originPolicy("")
	for origin, want := range map[string]bool{
		"https://www.bk-gmice.com":          true,
		"http://localhost:8080":             true,
		"https://evil.example":              false,
		"https://bk-gmice.com.evil.example": false,
	} {
		if ok(origin) != want {
			t.Errorf("origin %q: got %v want %v", origin, !want, want)
		}
	}
	h, _ := testHub(t)
	srv := httptest.NewServer(h.serve(originPolicy("")))
	defer srv.Close()
	conn, _, code := dial(t, srv, "https://evil.example")
	conn.Close()
	if code != http.StatusForbidden {
		t.Errorf("foreign origin got status %d, want 403", code)
	}
}
