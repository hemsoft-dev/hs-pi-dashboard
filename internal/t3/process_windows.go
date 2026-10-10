package t3

import (
	"context"
	"encoding/binary"
	"errors"
	"net/url"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var getTCPTable = windows.NewLazySystemDLL("iphlpapi.dll").NewProc("GetExtendedTcpTable")

func verifyRuntimeProcess(ctx context.Context, pid int, started time.Time, origin *url.URL) error {
	handle, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err
	}
	defer windows.CloseHandle(handle)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(handle, &created, &exited, &kernel, &user) != nil || !sameRuntimeStart(time.Unix(0, created.Nanoseconds()), started) {
		return errors.New("runtime process changed")
	}
	port, err := runtimePort(origin)
	if err != nil {
		return err
	}
	// IP Helper's owner-PID listener tables prove ownership without spawning
	// PowerShell or exposing process command lines.
	for _, family := range []uintptr{windows.AF_INET, windows.AF_INET6} {
		var size uint32
		getTCPTable.Call(0, uintptr(unsafe.Pointer(&size)), 0, family, 3, 0)
		if size < 4 || size > 16<<20 {
			continue
		}
		data := make([]byte, size)
		result, _, _ := getTCPTable.Call(uintptr(unsafe.Pointer(&data[0])), uintptr(unsafe.Pointer(&size)), 0, family, 3, 0)
		if result != 0 {
			continue
		}
		rowSize, portOffset, pidOffset := 24, 8, 20
		if family == windows.AF_INET6 {
			rowSize, portOffset, pidOffset = 56, 20, 52
		}
		count := int(binary.LittleEndian.Uint32(data[:4]))
		for i := 0; i < count && 4+(i+1)*rowSize <= len(data); i++ {
			row := data[4+i*rowSize : 4+(i+1)*rowSize]
			if int(binary.LittleEndian.Uint32(row[pidOffset:])) == pid && int(binary.BigEndian.Uint16(row[portOffset:])) == port {
				return ctx.Err()
			}
		}
	}
	return errors.New("runtime listener changed")
}
