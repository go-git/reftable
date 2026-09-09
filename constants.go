/*
Copyright 2020 Google LLC

Use of this source code is governed by a BSD-style
license that can be found in the LICENSE file or at
https://developers.google.com/open-source/licenses/bsd
*/

package reftable

var magic = [4]byte{'R', 'E', 'F', 'T'}

func headerSize(version int) int {
	if version == 1 {
		return 24

	}
	return 28
}

func footerSize(version int) int {
	if version == 1 {
		return 68
	}

	return 72
}

const defaultBlockSize = 4096

const blockTypeLog = 'g'
const blockTypeIndex = 'i'
const blockTypeRef = 'r'
const blockTypeObj = 'o'
const blockTypeAny = 0

const maxRestarts = (1 << 16) - 1

// maxDeflateRatio is the theoretical maximum expansion of a DEFLATE stream.
// Used to reject log blocks whose declared decompressed size cannot possibly
// be produced by the compressed bytes actually present.
const maxDeflateRatio = 1032

// maxIndexDepth bounds how many index levels a seek will descend. Offsets in
// index records are file-supplied and can form a cycle; a valid tree is only
// a few levels deep because each level holds strictly fewer blocks than the
// one below it.
const maxIndexDepth = 64
