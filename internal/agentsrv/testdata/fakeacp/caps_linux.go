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

package main

import (
	"errors"
	"fmt"
	"os"
	"strings"

	"golang.org/x/sys/unix"
)

// capabilities reports the process's user, its effective and bounding
// capabilities, and what creating a network device and a raw socket give.
func capabilities() string {
	status, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return "caps error: " + err.Error()
	}
	var sets []string
	for line := range strings.Lines(string(status)) {
		for _, set := range []string{"CapEff:", "CapBnd:"} {
			if v, ok := strings.CutPrefix(line, set); ok {
				sets = append(sets, strings.TrimSuffix(set, ":")+"="+strings.TrimSpace(v))
			}
		}
	}
	return fmt.Sprintf("uid=%d %s tun=%s raw=%s", os.Getuid(), strings.Join(sets, " "), result(createTUN()), result(rawSocket()))
}

func createTUN() (err error) {
	fd, err := unix.Open("/dev/net/tun", unix.O_RDWR, 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, unix.Close(fd)) }()
	ifr, err := unix.NewIfreq("tun9")
	if err != nil {
		return err
	}
	ifr.SetUint16(unix.IFF_TUN | unix.IFF_NO_PI)
	return unix.IoctlIfreq(fd, unix.TUNSETIFF, ifr)
}

func rawSocket() error {
	fd, err := unix.Socket(unix.AF_INET, unix.SOCK_RAW, unix.IPPROTO_ICMP)
	if err != nil {
		return err
	}
	return unix.Close(fd)
}

func result(err error) string {
	if err != nil {
		return err.Error()
	}
	return "ok"
}
