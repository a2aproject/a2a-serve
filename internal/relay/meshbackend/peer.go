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

package meshbackend

import (
	"strings"

	"github.com/ipfs/go-cid"
	mh "github.com/multiformats/go-multihash"
)

// maxPeerSegment bounds the peer segments the front decodes. The longest
// spelling of an agent's peer ID, base2, has about 330 characters.
const maxPeerSegment = 512

// canonicalPeer returns the base58 form of the peer ID that seg spells, as
// libp2p's peer.Decode reads it: a base58 multihash, or a CID of a libp2p
// key in any multibase. It reports false for a segment that isn't one.
func canonicalPeer(seg string) (string, bool) {
	if len(seg) > maxPeerSegment {
		return "", false
	}
	if strings.HasPrefix(seg, "Qm") || strings.HasPrefix(seg, "1") {
		m, err := mh.FromB58String(seg)
		if err != nil {
			return "", false
		}
		return m.B58String(), true
	}
	c, err := cid.Decode(seg)
	if err != nil || c.Type() != cid.Libp2pKey {
		return "", false
	}
	return c.Hash().B58String(), true
}
