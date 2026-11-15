//go:build !linux && !windows

package airplay

import "os/exec"

func setChildProcAttr(*exec.Cmd) {}

func afterChildStart(*exec.Cmd) {}
