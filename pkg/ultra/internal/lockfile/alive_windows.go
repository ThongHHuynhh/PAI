package lockfile

import "golang.org/x/sys/windows"

// stillActive is GetExitCodeProcess's code for a process that has not exited.
const stillActive = 259

// alive is whether process pid is running. When unsure it says yes, so a
// lock is never taken from a live holder on a guess.
func alive(pid int) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED // exists, but not ours to query
	}
	defer windows.CloseHandle(h)
	var code uint32
	if err := windows.GetExitCodeProcess(h, &code); err != nil {
		return true
	}
	return code == stillActive
}
