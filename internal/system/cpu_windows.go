package system

import (
	"golang.org/x/sys/windows"
	"unsafe"
)

var getSystemTimes = windows.NewLazySystemDLL("kernel32.dll").NewProc("GetSystemTimes")

func seconds(t windows.Filetime) float64 {
	return float64(uint64(t.HighDateTime)<<32|uint64(t.LowDateTime)) / 1e7
}
func cpuTimes() (float64, float64, float64, error) {
	var created, exited, kernel, user windows.Filetime
	if e := windows.GetProcessTimes(windows.CurrentProcess(), &created, &exited, &kernel, &user); e != nil {
		return 0, 0, 0, e
	}
	process := seconds(kernel) + seconds(user)
	var idle windows.Filetime
	ok, _, e := getSystemTimes.Call(uintptr(unsafe.Pointer(&idle)), uintptr(unsafe.Pointer(&kernel)), uintptr(unsafe.Pointer(&user)))
	if ok == 0 {
		return 0, 0, 0, e
	}
	return process, seconds(kernel) + seconds(user), seconds(idle), nil
}
