//go:build windows

package sysinfo

import (
	"unsafe"

	"golang.org/x/sys/windows"

	"github.com/sazda/network-monitor/internal/protocol"
)

var (
	kernel32                 = windows.NewLazySystemDLL("kernel32.dll")
	procGetSystemTimes       = kernel32.NewProc("GetSystemTimes")
	procGlobalMemoryStatusEx = kernel32.NewProc("GlobalMemoryStatusEx")
	procGetTickCount64       = kernel32.NewProc("GetTickCount64")
	procGetProcessMemoryInfo = kernel32.NewProc("K32GetProcessMemoryInfo")
)

func ft(f windows.Filetime) uint64 { return uint64(f.HighDateTime)<<32 | uint64(f.LowDateTime) }

// cpuTimes: vreme mirovanja i ukupno vreme (kernel uključuje i mirovanje).
func cpuTimes() (idle, all uint64, err error) {
	var i, k, u windows.Filetime
	r, _, e := procGetSystemTimes.Call(uintptr(unsafe.Pointer(&i)), uintptr(unsafe.Pointer(&k)), uintptr(unsafe.Pointer(&u)))
	if r == 0 {
		return 0, 0, e
	}
	return ft(i), ft(k) + ft(u), nil
}

type memoryStatusEx struct {
	Length               uint32
	MemoryLoad           uint32
	TotalPhys            uint64
	AvailPhys            uint64
	TotalPageFile        uint64
	AvailPageFile        uint64
	TotalVirtual         uint64
	AvailVirtual         uint64
	AvailExtendedVirtual uint64
}

func memory() (total, used uint64, err error) {
	var m memoryStatusEx
	m.Length = uint32(unsafe.Sizeof(m))
	r, _, e := procGlobalMemoryStatusEx.Call(uintptr(unsafe.Pointer(&m)))
	if r == 0 {
		return 0, 0, e
	}
	return m.TotalPhys, m.TotalPhys - m.AvailPhys, nil
}

func uptime() (uint64, error) {
	r, _, _ := procGetTickCount64.Call()
	return uint64(r) / 1000, nil
}

// disks: lokalni (fiksni) diskovi.
func disks() []protocol.Disk {
	var out []protocol.Disk
	mask, err := windows.GetLogicalDrives()
	if err != nil {
		return nil
	}
	for i := 0; i < 26; i++ {
		if mask&(1<<uint(i)) == 0 {
			continue
		}
		root := string(rune('A'+i)) + `:\`
		p, _ := windows.UTF16PtrFromString(root)
		if windows.GetDriveType(p) != windows.DRIVE_FIXED {
			continue
		}
		var free, total, totalFree uint64
		if windows.GetDiskFreeSpaceEx(p, &free, &total, &totalFree) != nil {
			continue
		}
		out = append(out, protocol.Disk{Name: root[:2], Total: total, Free: free})
	}
	return out
}

type processMemoryCounters struct {
	Cb                         uint32
	PageFaultCount             uint32
	PeakWorkingSetSize         uintptr
	WorkingSetSize             uintptr
	QuotaPeakPagedPoolUsage    uintptr
	QuotaPagedPoolUsage        uintptr
	QuotaPeakNonPagedPoolUsage uintptr
	QuotaNonPagedPoolUsage     uintptr
	PagefileUsage              uintptr
	PeakPagefileUsage          uintptr
}

// processes: svi procesi koje servis (LocalSystem) može da otvori; vreme
// procesora je kernel+user u jedinicama od 100 ns (kao GetSystemTimes),
// memorija je radni skup.
func processes() (map[procKey]procSample, error) {
	snap, err := windows.CreateToolhelp32Snapshot(windows.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return nil, err
	}
	defer windows.CloseHandle(snap)
	out := map[procKey]procSample{}
	var e windows.ProcessEntry32
	e.Size = uint32(unsafe.Sizeof(e))
	for err = windows.Process32First(snap, &e); err == nil; err = windows.Process32Next(snap, &e) {
		if e.ProcessID == 0 { // „System Idle Process”
			continue
		}
		h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, e.ProcessID)
		if err != nil {
			continue
		}
		var c, x, k, u windows.Filetime
		if windows.GetProcessTimes(h, &c, &x, &k, &u) != nil {
			windows.CloseHandle(h)
			continue
		}
		var pmc processMemoryCounters
		pmc.Cb = uint32(unsafe.Sizeof(pmc))
		if r, _, _ := procGetProcessMemoryInfo.Call(uintptr(h), uintptr(unsafe.Pointer(&pmc)), uintptr(pmc.Cb)); r == 0 {
			pmc.WorkingSetSize = 0
		}
		windows.CloseHandle(h)
		name := windows.UTF16ToString(e.ExeFile[:])
		out[procKey{e.ProcessID, name}] = procSample{cpu: ft(k) + ft(u), mem: uint64(pmc.WorkingSetSize)}
	}
	return out, nil
}
