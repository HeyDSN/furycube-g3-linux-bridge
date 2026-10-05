package main

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unsafe"
)

// Connection method byte the web driver expects in every frame.
const (
	connWired    = 0x80
	connWireless = 0x81
)

// USB ids seen in the vendor's Windows app: dongle (2.4G) and cable.
var knownDevices = map[string]byte{
	"00001D57:0000FA60": connWireless,
	"00001D57:00002266": connWired,
}

// featureSizes parses a HID report descriptor and returns, per report ID, the size in bytes
// of its Feature report including the leading report ID byte (what HIDIOCSFEATURE needs).
func featureSizes(desc []byte) map[byte]int {
	sizes := map[byte]int{}
	var (
		reportID          byte
		reportSize, count int
	)
	for i := 0; i < len(desc); {
		prefix := desc[i]
		if prefix == 0xFE { // long item: skip
			if i+1 >= len(desc) {
				break
			}
			i += 3 + int(desc[i+1])
			continue
		}
		n := int(prefix & 0x03)
		if n == 3 {
			n = 4
		}
		if i+1+n > len(desc) {
			break
		}
		var val int
		for j := 0; j < n; j++ {
			val |= int(desc[i+1+j]) << (8 * j)
		}
		typ, tag := (prefix>>2)&0x03, prefix>>4
		switch {
		case typ == 1 && tag == 7: // global Report Size
			reportSize = val
		case typ == 1 && tag == 8: // global Report ID
			reportID = byte(val)
		case typ == 1 && tag == 9: // global Report Count
			count = val
		case typ == 0 && tag == 11: // main Feature
			sizes[reportID] += reportSize * count
		}
		i += 1 + n
	}
	out := make(map[byte]int, len(sizes))
	for id, bits := range sizes {
		out[id] = (bits+7)/8 + 1
	}
	return out
}

// deviceInfo describes the hidraw node that carries the vendor configuration interface.
type deviceInfo struct {
	path  string
	conn  byte
	sizes map[byte]int
}

// findDevice scans /sys/class/hidraw for the config interface of a known device. That is
// the interface whose descriptor declares the DPI (ID 4) and button (ID 8) feature reports.
func findDevice(sysRoot string) *deviceInfo {
	nodes, _ := filepath.Glob(filepath.Join(sysRoot, "hidraw*"))
	for _, node := range nodes {
		dev := filepath.Join(node, "device")
		uevent, err := os.ReadFile(filepath.Join(dev, "uevent"))
		if err != nil {
			continue
		}
		var conn byte
		for id, c := range knownDevices {
			if strings.Contains(string(uevent), "HID_ID=0003:"+id) {
				conn = c
			}
		}
		if conn == 0 {
			continue
		}
		desc, err := os.ReadFile(filepath.Join(dev, "report_descriptor"))
		if err != nil {
			continue
		}
		sizes := featureSizes(desc)
		if sizes[0x04] == 56 && sizes[0x08] == 59 {
			return &deviceInfo{path: "/dev/" + filepath.Base(node), conn: conn, sizes: sizes}
		}
	}
	return nil
}

// ioctl numbers from linux/hidraw.h: _IOC(_IOC_READ|_IOC_WRITE, 'H', 6, len)
func hidiocsfeature(n int) uintptr { return 0xC0004806 | uintptr(n)<<16 }

// setFeature sends one feature report (report ID first) through the open hidraw file.
func setFeature(f *os.File, report []byte) error {
	rc, err := f.SyscallConn()
	if err != nil {
		return err
	}
	buf := append([]byte(nil), report...)
	var errno syscall.Errno
	if err := rc.Control(func(fd uintptr) {
		_, _, errno = syscall.Syscall(syscall.SYS_IOCTL, fd, hidiocsfeature(len(buf)),
			uintptr(unsafe.Pointer(&buf[0])))
	}); err != nil {
		return err
	}
	if errno != 0 {
		return fmt.Errorf("HIDIOCSFEATURE: %w", errno)
	}
	return nil
}
