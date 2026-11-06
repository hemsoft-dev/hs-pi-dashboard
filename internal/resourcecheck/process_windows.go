package resourcecheck

import (
	"os"
	"syscall"
	"unsafe"
)

// HandleName names what Handles counts on this platform.
const HandleName = "open handles"

// HandlesPerThread is what one Go OS thread holds on Windows 11: about 6 for
// its thread handle and the runtime's per-thread events and timer, and 2 more
// Winsock keeps once the thread has used a socket, as collector threads do.
// Idle threads measured 5.9-6.3 handles each, socket-using threads 7.8-8.1
// (Home, six runs of 20+ threads). TestHandlesPerThread rechecks it.
const HandlesPerThread = 8

var getProcessHandleCount = syscall.NewLazyDLL("kernel32.dll").NewProc("GetProcessHandleCount")

func openHandles() int {
	process, err := syscall.GetCurrentProcess()
	if err != nil {
		return Unmeasured
	}
	var n uint32
	if ok, _, _ := getProcessHandleCount.Call(uintptr(process), uintptr(unsafe.Pointer(&n))); ok == 0 {
		return Unmeasured
	}
	return int(n)
}

// childProcesses counts running processes whose parent is this one. Windows
// lists only running processes; an exited child whose handle was never closed
// shows up in openHandles instead.
func childProcesses() int {
	snapshot, err := syscall.CreateToolhelp32Snapshot(syscall.TH32CS_SNAPPROCESS, 0)
	if err != nil {
		return Unmeasured
	}
	defer syscall.CloseHandle(snapshot)
	self := uint32(os.Getpid())
	var entry syscall.ProcessEntry32
	entry.Size = uint32(unsafe.Sizeof(entry))
	children := 0
	for err = syscall.Process32First(snapshot, &entry); err == nil; err = syscall.Process32Next(snapshot, &entry) {
		if entry.ParentProcessID == self {
			children++
		}
	}
	return children
}
