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

// Command probe runs in the containers of the tests of the sandbox package
// and prints, one line per argument, what it finds: "read:<path>" prints
// the file, "ls:<dir>" its entries, and "dial:<host:port>" whether a TCP
// connection opens.
package main

import (
	"fmt"
	"net"
	"os"
	"strings"
	"time"
)

func main() {
	for _, arg := range os.Args[1:] {
		kind, target, _ := strings.Cut(arg, ":")
		fmt.Println(arg + " " + probe(kind, target))
	}
}

func probe(kind, target string) string {
	switch kind {
	case "read":
		data, err := os.ReadFile(target)
		if err != nil {
			return "error"
		}
		return strings.TrimSpace(string(data))
	case "ls":
		entries, err := os.ReadDir(target)
		if err != nil {
			return "error"
		}
		return fmt.Sprintf("%d entries", len(entries))
	case "dial":
		c, err := net.DialTimeout("tcp", target, 10*time.Second)
		if err != nil {
			return "refused"
		}
		if err := c.Close(); err != nil {
			return "refused"
		}
		return "ok"
	}
	return "unknown"
}
