// furycube-g3-linux-bridge replaces the vendor's Windows helper for the Furycube G3 web driver.
//
// The web driver (https://www.bk-gmice.com) talks to a helper on ws://localhost:10086 using
// hex text frames. This program speaks that protocol and forwards configuration reports to
// the mouse's hidraw interface, so the official web UI works on Linux without Windows.
//
// Run it with sudo, open the web driver, change settings, close it again.
package main

import (
	"encoding/hex"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

const (
	listenPort      = "10086"
	sysHidraw       = "/sys/class/hidraw"
	helperVersion   = "0305" // major 3, minor 5: what the web driver expects from a current helper
	g3DeviceID      = 0x66   // 102, "G3" in the web driver's device table
	statusReportID  = 0x03   // heartbeat from the dongle: 03 66 41 <state> <battery %>
	ackCmd          = 0x50
	frameCmdPrefix  = 0xF0 // helper commands (version, bulk read); not HID reports
	defaultOrigins  = "https://www.bk-gmice.com,https://bk-gmice.com"
	scanInterval    = time.Second
	hidReadBufBytes = 64
)

// Report IDs the bridge will forward. These are the settings the G3 page writes: DPI (4),
// lighting/sleep (5), polling rate (6), buttons (8), macros (9) and the 0x0C control report.
// Everything else, in particular the calibration/firmware reports (0xA0, 0x22), is refused.
var allowedReports = map[byte]bool{0x04: true, 0x05: true, 0x06: true, 0x08: true, 0x09: true, 0x0C: true}

type hub struct {
	mu      sync.Mutex
	clients map[*wsConn]struct{}
	dev     *deviceInfo
	file    *os.File
	status  []byte // last presence frame, replayed to new clients
	verbose bool
	writeFn func(*os.File, []byte) error // setFeature; replaceable in tests
}

func newHub(verbose bool) *hub {
	return &hub{clients: map[*wsConn]struct{}{}, verbose: verbose, writeFn: setFeature}
}

func (h *hub) vlog(format string, args ...any) {
	if h.verbose {
		log.Printf(format, args...)
	}
}

func (h *hub) addClient(c *wsConn) {
	h.mu.Lock()
	h.clients[c] = struct{}{}
	status := h.status
	h.mu.Unlock()
	c.writeText(helperVersion)
	if status != nil {
		c.writeText(hex.EncodeToString(status))
	}
}

func (h *hub) removeClient(c *wsConn) {
	h.mu.Lock()
	delete(h.clients, c)
	h.mu.Unlock()
}

func (h *hub) broadcast(frame []byte) {
	h.mu.Lock()
	clients := make([]*wsConn, 0, len(h.clients))
	for c := range h.clients {
		clients = append(clients, c)
	}
	h.mu.Unlock()
	h.vlog("-> clients %x", frame)
	text := hex.EncodeToString(frame)
	for _, c := range clients {
		c.writeText(text)
	}
}

// handleText processes one frame from the web driver and replies on the same connection.
func (h *hub) handleText(c *wsConn, text []byte) {
	frame, err := hex.DecodeString(strings.TrimSpace(string(text)))
	if err != nil || len(frame) < 3 {
		h.vlog("ignored malformed frame %q", text)
		return
	}
	h.vlog("<- web    %x", frame)
	if frame[0] == frameCmdPrefix {
		// F0 <conn> <dev> <cmd> 00: version/bulk-read requests. The G3 page does not use them
		// (it is on the driver's "no read-back" list), and the mouse offers no read path we know.
		return
	}
	conn, dev, report := frame[0], frame[1], frame[2:]
	ok := h.forward(dev, report)
	status := byte(0)
	if !ok {
		status = 1
	}
	c.writeText(hex.EncodeToString([]byte{conn, dev, ackCmd, status, 0}))
}

// forward writes one configuration report to the mouse. The web driver builds buffers a few
// bytes longer than the report, so the payload is cut or zero-padded to the exact size the
// device's descriptor declares for that report ID.
func (h *hub) forward(dev byte, report []byte) bool {
	if dev != g3DeviceID {
		log.Printf("refused: unsupported device id %#x", dev)
		return false
	}
	id := report[0]
	if !allowedReports[id] {
		log.Printf("refused: report %#x not on the allow list", id)
		return false
	}
	h.mu.Lock()
	file, info := h.file, h.dev
	h.mu.Unlock()
	if file == nil {
		log.Printf("dropped report %#x: mouse not connected", id)
		return false
	}
	size := info.sizes[id]
	if size == 0 {
		log.Printf("refused: device does not declare feature report %#x", id)
		return false
	}
	pkt := make([]byte, size)
	copy(pkt, report)
	if err := h.writeFn(file, pkt); err != nil {
		log.Printf("write report %#x failed: %v", id, err)
		return false
	}
	log.Printf("sent report %#x (%d bytes)", id, size)
	return true
}

// monitor keeps the hidraw node open while the mouse is plugged in and relays its
// heartbeat (connection + battery) to the web driver as presence frames.
func (h *hub) monitor() {
	var lastErr string
	for {
		time.Sleep(scanInterval)
		h.mu.Lock()
		attached := h.file != nil
		h.mu.Unlock()
		if attached {
			continue
		}
		info := findDevice(sysHidraw)
		if info == nil {
			continue
		}
		f, err := os.OpenFile(info.path, os.O_RDWR, 0)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				log.Printf("cannot open %s: %v (run the bridge with sudo)", info.path, err)
				lastErr = msg
			}
			continue
		}
		lastErr = ""
		h.mu.Lock()
		h.file, h.dev = f, info
		h.mu.Unlock()
		log.Printf("mouse attached on %s", info.path)
		go h.readLoop(f, info)
	}
}

func (h *hub) readLoop(f *os.File, info *deviceInfo) {
	buf := make([]byte, hidReadBufBytes)
	for {
		n, err := f.Read(buf)
		if err != nil {
			break
		}
		if n >= 5 && buf[0] == statusReportID && buf[1] == g3DeviceID {
			frame := append([]byte{info.conn, 0x01}, buf[1:5]...)
			h.mu.Lock()
			h.status = frame
			h.mu.Unlock()
			h.broadcast(frame)
		}
	}
	f.Close()
	h.mu.Lock()
	h.file, h.dev, h.status = nil, nil, nil
	h.mu.Unlock()
	log.Printf("mouse detached")
	h.broadcast([]byte{info.conn, 0x00, g3DeviceID})
}

func (h *hub) serve(originOK func(string) bool) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := upgrade(w, r, originOK)
		if err != nil {
			log.Printf("rejected connection from %s: %v", r.RemoteAddr, err)
			return
		}
		defer c.close()
		log.Printf("web driver connected (%s)", r.Header.Get("Origin"))
		h.addClient(c)
		defer h.removeClient(c)
		for {
			msg, err := c.readMessage()
			if err != nil {
				log.Printf("web driver disconnected")
				return
			}
			h.handleText(c, msg)
		}
	}
}

func originPolicy(extra string) func(string) bool {
	allowed := map[string]bool{}
	for _, o := range strings.Split(defaultOrigins+","+extra, ",") {
		if o = strings.TrimSpace(o); o != "" {
			allowed[strings.TrimSuffix(o, "/")] = true
		}
	}
	return func(origin string) bool {
		if allowed[origin] {
			return true
		}
		u, err := url.Parse(origin)
		return err == nil && (u.Hostname() == "localhost" || u.Hostname() == "127.0.0.1")
	}
}

func main() {
	verbose := flag.Bool("v", false, "log every frame")
	extra := flag.String("allow-origin", "", "extra comma-separated Origins allowed to connect")
	asUser := flag.String("user", "", "when started as root: user to run the bridge as (default: the sudo caller)")
	flag.Parse()

	// Under sudo, root only grants temporary hidraw access; the bridge runs as the caller.
	if os.Geteuid() == 0 {
		os.Exit(runBroker(*asUser))
	}

	h := newHub(*verbose)
	go h.monitor()
	srv := &http.Server{Handler: h.serve(originPolicy(*extra))}

	// Browsers resolve "localhost" to either family, so listen on both loopbacks.
	var listened int
	for _, addr := range []string{"127.0.0.1:" + listenPort, "[::1]:" + listenPort} {
		l, err := net.Listen("tcp", addr)
		if err != nil {
			log.Printf("listen %s: %v", addr, err)
			continue
		}
		listened++
		go srv.Serve(l)
	}
	if listened == 0 {
		fmt.Fprintln(os.Stderr, "could not listen on port "+listenPort+" (is the vendor helper or another bridge running?)")
		os.Exit(1)
	}
	log.Printf("furycube-g3-linux-bridge ready on ws://localhost:%s, open https://www.bk-gmice.com", listenPort)

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, os.Interrupt, syscall.SIGTERM)
	<-sig
}
