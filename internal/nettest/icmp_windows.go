//go:build windows

package nettest

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"syscall"
	"time"
	"unsafe"
)

// Na Windows-u se koristi IcmpSendEcho iz iphlpapi.dll, koji ne zahteva
// administratorska prava (za razliku od raw socket-a).
var (
	iphlpapi          = syscall.NewLazyDLL("iphlpapi.dll")
	procIcmpCreate    = iphlpapi.NewProc("IcmpCreateFile")
	procIcmpClose     = iphlpapi.NewProc("IcmpCloseHandle")
	procIcmpSendEcho  = iphlpapi.NewProc("IcmpSendEcho")
	icmpPayload       = []byte("network-monitor-ping-0123456789ab")
	invalidHandle     = ^uintptr(0)
	ipStatusSuccess   = uint32(0)
	ipStatusTimedOut  = uint32(11010)
	replyBufferExtras = 256
)

type winPinger struct{ h uintptr }

func newPinger() (pinger, error) {
	if err := procIcmpCreate.Find(); err != nil {
		return nil, errPingUnsupported
	}
	h, _, err := procIcmpCreate.Call()
	if h == invalidHandle || h == 0 {
		return nil, fmt.Errorf("IcmpCreateFile: %v", err)
	}
	return &winPinger{h: h}, nil
}

func (p *winPinger) Close() { procIcmpClose.Call(p.h) }

func (p *winPinger) Ping(ip net.IP, _ int, timeout time.Duration) (time.Duration, error) {
	// IPAddr je u mrežnom redosledu bajtova, smeštenom u uint32 na
	// little-endian mašini.
	addr := binary.LittleEndian.Uint32(ip.To4())
	reply := make([]byte, 64+len(icmpPayload)+replyBufferExtras)
	start := time.Now()
	n, _, _ := procIcmpSendEcho.Call(
		p.h,
		uintptr(addr),
		uintptr(unsafe.Pointer(&icmpPayload[0])),
		uintptr(len(icmpPayload)),
		0,
		uintptr(unsafe.Pointer(&reply[0])),
		uintptr(len(reply)),
		uintptr(timeout.Milliseconds()),
	)
	// RoundTripTime iz odgovora ima rezoluciju od 1 ms, što je u LAN-u
	// premalo; zato merimo sami (IcmpSendEcho je sinhron).
	rtt := time.Since(start)
	if n == 0 {
		return 0, errors.New("nema odgovora")
	}
	// ICMP_ECHO_REPLY: Address uint32, Status uint32, ...
	if status := binary.LittleEndian.Uint32(reply[4:8]); status != ipStatusSuccess {
		if status == ipStatusTimedOut {
			return 0, errors.New("isteklo vreme")
		}
		return 0, fmt.Errorf("ICMP status %d", status)
	}
	return rtt, nil
}
