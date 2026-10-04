// Copyright 2009 The Go Authors. All rights reserved.
// Portions Copyright 2016 Hiroshi Ioka. All rights reserved.
//
// Redistribution and use in source and binary forms, with or without
// modification, are permitted provided that the following conditions are
// met:
//
//    * Redistributions of source code must retain the above copyright
// notice, this list of conditions and the following disclaimer.
//    * Redistributions in binary form must reproduce the above
// copyright notice, this list of conditions and the following disclaimer
// in the documentation and/or other materials provided with the
// distribution.
//    * Neither the name of Google Inc. nor the names of its
// contributors may be used to endorse or promote products derived from
// this software without specific prior written permission.
//
// THIS SOFTWARE IS PROVIDED BY THE COPYRIGHT HOLDERS AND CONTRIBUTORS
// "AS IS" AND ANY EXPRESS OR IMPLIED WARRANTIES, INCLUDING, BUT NOT
// LIMITED TO, THE IMPLIED WARRANTIES OF MERCHANTABILITY AND FITNESS FOR
// A PARTICULAR PURPOSE ARE DISCLAIMED. IN NO EVENT SHALL THE COPYRIGHT
// OWNER OR CONTRIBUTORS BE LIABLE FOR ANY DIRECT, INDIRECT, INCIDENTAL,
// SPECIAL, EXEMPLARY, OR CONSEQUENTIAL DAMAGES (INCLUDING, BUT NOT
// LIMITED TO, PROCUREMENT OF SUBSTITUTE GOODS OR SERVICES; LOSS OF USE,
// DATA, OR PROFITS; OR BUSINESS INTERRUPTION) HOWEVER CAUSED AND ON ANY
// THEORY OF LIABILITY, WHETHER IN CONTRACT, STRICT LIABILITY, OR TORT
// (INCLUDING NEGLIGENCE OR OTHERWISE) ARISING IN ANY WAY OUT OF THE USE
// OF THIS SOFTWARE, EVEN IF ADVISED OF THE POSSIBILITY OF SUCH DAMAGE.

// Modified for s3-smb, 2026. See docs/vendored.md.
package crypt

import "crypto/cipher"

// aesCMAC computes the RFC 4493 AES-CMAC of message. The caller supplies an
// AES block from aes.NewCipher. Subkeys and digest state are local
// to each call. The last complete block uses K1; a partial block uses K2 and
// the RFC 4493 padding, including for the empty message.
func aesCMAC(block cipher.Block, message []byte) [16]byte {
	var subkey [16]byte
	block.Encrypt(subkey[:], subkey[:])
	k1 := doubleCMAC(subkey)
	k2 := doubleCMAC(k1)
	var digest [16]byte
	for len(message) > 16 {
		for i := range digest {
			digest[i] ^= message[i]
		}
		block.Encrypt(digest[:], digest[:])
		message = message[16:]
	}
	lastKey := k1
	if len(message) < 16 {
		lastKey = k2
		digest[len(message)] ^= 0x80
	}
	for i, value := range message {
		digest[i] ^= value
	}
	for i := range digest {
		digest[i] ^= lastKey[i]
	}
	block.Encrypt(digest[:], digest[:])
	return digest
}

func doubleCMAC(input [16]byte) [16]byte {
	var output [16]byte
	var carry byte
	for i := len(input) - 1; i >= 0; i-- {
		output[i] = input[i]<<1 | carry
		carry = input[i] >> 7
	}
	output[15] ^= 0x87 * carry
	return output
}
