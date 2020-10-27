/*
 * Copyright 2020 Saffat Technologies, Ltd.
 *
 * Licensed under the Apache License, Version 2.0 (the "License");
 * you may not use this file except in compliance with the License.
 * You may obtain a copy of the License at
 *
 *     http://www.apache.org/licenses/LICENSE-2.0
 *
 * Unless required by applicable law or agreed to in writing, software
 * distributed under the License is distributed on an "AS IS" BASIS,
 * WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
 * See the License for the specific language governing permissions and
 * limitations under the License.
 */

package unitdb

import (
	"io"
	"sort"
	"time"

	"github.com/unit-io/bpool"
)

type _WindowWriter struct {
	timeWindowBucket *_TimeWindowBucket
	winBlocks        map[int32]_WinBlock // map[windowIdx]winBlock

	buffer *bpool.Buffer

	leasing map[int32][]uint64 // map[blockIdx][]seq
}

func newWindowWriter(tw *_TimeWindowBucket, buf *bpool.Buffer) *_WindowWriter {
	return &_WindowWriter{winBlocks: make(map[int32]_WinBlock), timeWindowBucket: tw, buffer: buf, leasing: make(map[int32][]uint64)}
}

func (w *_WindowWriter) del(seq uint64, bIdx int32) error {
	off := int64(blockSize * uint32(bIdx))
	h := _WindowHandle{file: w.timeWindowBucket.file, offset: off}
	if err := h.read(); err != nil {
		return err
	}
	entryIdx := -1
	for i := 0; i < int(h.winBlock.entryIdx); i++ {
		e := h.winBlock.entries[i]
		if e.sequence == seq { //record exist in db.
			entryIdx = i
			break
		}
	}
	if entryIdx == -1 {
		return nil // no entry in db to delete.
	}
	h.winBlock.entryIdx--

	i := entryIdx
	for ; i < entriesPerIndexBlock-1; i++ {
		h.winBlock.entries[i] = h.winBlock.entries[i+1]
	}
	h.winBlock.entries[i] = _WinEntry{}

	return nil
}

// append appends window entries to buffer.
func (w *_WindowWriter) append(topicHash uint64, off int64, wEntries _WindowEntries) (newOff int64, err error) {
	var b _WinBlock
	var ok bool
	var winIdx int32
	if off == 0 {
		w.timeWindowBucket.timeInfo.windowIdx++
		winIdx = w.timeWindowBucket.timeInfo.windowIdx
	} else {
		winIdx = int32(off / int64(blockSize))
	}
	b, ok = w.winBlocks[winIdx]
	if !ok && off > 0 {
		if winIdx <= w.timeWindowBucket.timeInfo.windowIdx {
			h := _WindowHandle{file: w.timeWindowBucket.file, offset: off}
			if err := h.read(); err != nil && err != io.EOF {
				return off, err
			}
			b = h.winBlock
			b.validation(topicHash)
			b.leased = true
		}
	}
	b.topicHash = topicHash
	for _, we := range wEntries {
		if we.sequence == 0 {
			continue
		}
		entryIdx := 0
		for i := 0; i < seqsPerWindowBlock; i++ {
			e := b.entries[i]
			if e.sequence == we.sequence { //record exist in db.
				entryIdx = -1
				break
			}
		}
		if entryIdx == -1 {
			continue
		}
		if b.entryIdx == seqsPerWindowBlock {
			topicHash := b.topicHash
			next := int64(blockSize * uint32(winIdx))
			// set approximate cutoff on winBlock.
			b.cutoffTime = time.Now().Unix()
			w.winBlocks[winIdx] = b
			w.timeWindowBucket.timeInfo.windowIdx++
			winIdx = w.timeWindowBucket.timeInfo.windowIdx
			b = _WinBlock{topicHash: topicHash, next: next}
		}
		if b.leased {
			w.leasing[winIdx] = append(w.leasing[winIdx], we.sequence)
		}
		b.entries[b.entryIdx] = _WinEntry{sequence: we.sequence, expiresAt: we.expiresAt}
		b.dirty = true
		b.entryIdx++
	}

	w.winBlocks[winIdx] = b
	return int64(blockSize * uint32(winIdx)), nil
}

func (w *_WindowWriter) write() error {
	for bIdx, win := range w.winBlocks {
		if !win.leased || !win.dirty {
			continue
		}
		off := int64(blockSize * uint32(bIdx))
		if _, err := w.timeWindowBucket.file.WriteAt(win.MarshalBinary(), off); err != nil {
			return err
		}
		win.dirty = false
		w.winBlocks[bIdx] = win
	}

	// sort blocks by blockIdx.
	var blockIdx []int32
	for bIdx := range w.winBlocks {
		if w.winBlocks[bIdx].leased || !w.winBlocks[bIdx].dirty {
			continue
		}
		blockIdx = append(blockIdx, bIdx)
	}

	sort.Slice(blockIdx, func(i, j int) bool { return blockIdx[i] < blockIdx[j] })

	winBlocks, err := blockRange(blockIdx)
	if err != nil {
		return err
	}
	bufOff := int64(0)
	for _, blocks := range winBlocks {
		if len(blocks) == 1 {
			bIdx := blocks[0]
			off := int64(blockSize * uint32(bIdx))
			wb := w.winBlocks[bIdx]
			buf := wb.MarshalBinary()
			if _, err := w.timeWindowBucket.file.WriteAt(buf, off); err != nil {
				return err
			}
			wb.dirty = false
			w.winBlocks[bIdx] = wb
			continue
		}
		blockOff := int64(blockSize * uint32(blocks[0]))
		for bIdx := blocks[0]; bIdx <= blocks[1]; bIdx++ {
			wb := w.winBlocks[bIdx]
			w.buffer.Write(wb.MarshalBinary())
			wb.dirty = false
			w.winBlocks[bIdx] = wb
		}
		blockData, err := w.buffer.Slice(bufOff, w.buffer.Size())
		if err != nil {
			return err
		}
		if _, err := w.timeWindowBucket.file.WriteAt(blockData, blockOff); err != nil {
			return err
		}
		bufOff = w.buffer.Size()
	}
	return nil
}

func (w *_WindowWriter) rollback() error {
	for bIdx, seqs := range w.leasing {
		for _, seq := range seqs {
			if err := w.del(seq, bIdx); err != nil {
				return err
			}
		}
	}
	return nil
}