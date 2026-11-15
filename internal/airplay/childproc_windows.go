//go:build windows

package airplay

import (
	"os/exec"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

const createNoWindow = 0x08000000

// setChildProcAttr prevents a console window for the capture child. The tray
// application has no console, and FFmpeg must not open one.
func setChildProcAttr(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true, CreationFlags: createNoWindow}
}

var (
	killJobOnce sync.Once
	killJob     windows.Handle
)

// killOnExitJob returns a job object that stops all its processes when this
// process exits. Windows closes the job handle at process exit, also after a
// crash, so FFmpeg cannot continue to run without its parent.
func killOnExitJob() windows.Handle {
	killJobOnce.Do(func() {
		job, err := windows.CreateJobObject(nil, nil)
		if err != nil {
			return
		}
		info := windows.JOBOBJECT_EXTENDED_LIMIT_INFORMATION{}
		info.BasicLimitInformation.LimitFlags = windows.JOB_OBJECT_LIMIT_KILL_ON_JOB_CLOSE
		if _, err := windows.SetInformationJobObject(job, windows.JobObjectExtendedLimitInformation,
			uintptr(unsafe.Pointer(&info)), uint32(unsafe.Sizeof(info))); err != nil {
			_ = windows.CloseHandle(job)
			return
		}
		killJob = job
	})
	return killJob
}

func afterChildStart(cmd *exec.Cmd) {
	job := killOnExitJob()
	if job == 0 || cmd.Process == nil {
		return
	}
	process, err := windows.OpenProcess(windows.PROCESS_SET_QUOTA|windows.PROCESS_TERMINATE, false, uint32(cmd.Process.Pid))
	if err != nil {
		return
	}
	defer windows.CloseHandle(process)
	if err := windows.AssignProcessToJobObject(job, process); err != nil {
		dbg("[CAPTURE] could not add the capture process to the job object: %v", err)
	}
}
