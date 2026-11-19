// Copyright 2026 The A2A Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

//go:build linux

package boundary

import (
	"errors"
	"fmt"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// refusedSyscalls fail with EPERM under [confine]: the system calls that
// make or enter namespaces, mount, or reach keyrings, bpf, perf events and
// userfaultfd, which Docker's default seccomp profile refuses a container
// without CAP_SYS_ADMIN, and the ones that read or change another process,
// which could reach a process of the sandbox that has no filter, such as
// bubblewrap's, where ptrace_scope allows it.
var refusedSyscalls = []uint32{
	unix.SYS_UNSHARE, unix.SYS_SETNS,
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT,
	unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT, unix.SYS_MOUNT_SETATTR,
	unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN, unix.SYS_USERFAULTFD,
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV, unix.SYS_KCMP,
}

// cloneNamespaces are the flags of clone that make namespaces.
const cloneNamespaces = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC |
	unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET

// x32Bit marks the system calls of the x32 ABI on amd64.
const x32Bit = 0x40000000

// confine installs, on every thread of the launcher and so on the agent it
// starts, a seccomp filter that refuses what Docker's default profile
// refuses a container without CAP_SYS_ADMIN, and which Docker lets through
// once the container has it for the FUSE mount: [refusedSyscalls], and
// clone with namespace flags. clone3 fails with ENOSYS, since a filter
// can't read its flags, so that libc falls back to clone. System calls of
// another architecture or ABI fail with ENOSYS.
func confine() error {
	if nativeAuditArch == 0 {
		return errors.New("the launcher has no seccomp filter for this architecture")
	}
	prog := seccompProgram()
	if err := prctlAll(unix.PR_SET_NO_NEW_PRIVS, 1); err != nil {
		return fmt.Errorf("no_new_privs: %w", err)
	}
	fprog := unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}
	if _, _, errno := syscall.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(&fprog))); errno != 0 {
		return fmt.Errorf("installing the seccomp filter: %w", errno)
	}
	return nil
}

// seccompProgram assembles the filter of [confine]. The offsets are those
// of struct seccomp_data, with the low word of the first argument at 16 on
// the little-endian architectures it supports.
func seccompProgram() []unix.SockFilter {
	const (
		nr, arch, arg0 = 0, 4, 16
		allow          = unix.SECCOMP_RET_ALLOW
		eperm          = unix.SECCOMP_RET_ERRNO | uint32(unix.EPERM)
		enosys         = unix.SECCOMP_RET_ERRNO | uint32(unix.ENOSYS)
	)
	ld := func(off uint32) unix.SockFilter {
		return unix.SockFilter{Code: unix.BPF_LD | unix.BPF_W | unix.BPF_ABS, K: off}
	}
	ret := func(v uint32) unix.SockFilter { return unix.SockFilter{Code: unix.BPF_RET | unix.BPF_K, K: v} }
	// jump jumps to the instruction at index to when the condition holds,
	// which the caller fixes once the returns have their places.
	type jump struct {
		at int
		to *int
	}
	var prog []unix.SockFilter
	var jumps []jump
	var retEPERM, retENOSYS, cloneCheck int
	cond := func(code uint16, k uint32, to *int) {
		jumps = append(jumps, jump{at: len(prog), to: to})
		prog = append(prog, unix.SockFilter{Code: unix.BPF_JMP | code | unix.BPF_K, K: k})
	}
	var archOK int
	prog = append(prog, ld(arch))
	cond(unix.BPF_JEQ, nativeAuditArch, &archOK)
	prog = append(prog, ret(enosys))
	archOK = len(prog)
	prog = append(prog, ld(nr))
	cond(unix.BPF_JGE, x32Bit, &retENOSYS)
	for _, sc := range refusedSyscalls {
		cond(unix.BPF_JEQ, sc, &retEPERM)
	}
	cond(unix.BPF_JEQ, unix.SYS_CLONE3, &retENOSYS)
	cond(unix.BPF_JEQ, unix.SYS_CLONE, &cloneCheck)
	prog = append(prog, ret(allow))
	cloneCheck = len(prog)
	prog = append(prog, ld(arg0))
	cond(unix.BPF_JSET, cloneNamespaces, &retEPERM)
	prog = append(prog, ret(allow))
	retEPERM = len(prog)
	prog = append(prog, ret(eperm))
	retENOSYS = len(prog)
	prog = append(prog, ret(enosys))
	for _, j := range jumps {
		prog[j.at].Jt = uint8(*j.to - j.at - 1)
	}
	return prog
}
