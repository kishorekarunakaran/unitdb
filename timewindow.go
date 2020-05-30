package unitdb

import (
	"encoding/binary"
	"fmt"
	"io"
	"sort"
	"sync"
	"time"

	"github.com/unit-io/bpool"
	"github.com/unit-io/unitdb/hash"
)

type (
	winEntry struct {
		topicHash uint64 // topicHash used in DB query and not persisted
		seq       uint64
		expiresAt uint32
	}
	winBlock struct {
		topicHash uint64
		entries   [seqsPerWindowBlock]winEntry
		next      int64 //next stores offset that links multiple winBlocks for a topic hash. Most recent offset is stored into the trie to iterate entries in reverse order)
		cutoff    int64
		entryIdx  uint16

		dirty  bool // dirty used during timeWindow append and not persisted
		leased bool // leased used in timeWindow write and not persisted
	}
)

func (e winEntry) Seq() uint64 {
	return e.seq
}

func (e winEntry) ExpiresAt() uint32 {
	return e.expiresAt
}

func (e winEntry) isExpired() bool {
	return e.expiresAt != 0 && e.expiresAt <= uint32(time.Now().Unix())
}

func (w winBlock) Cutoff(cutoff int64) bool {
	return w.cutoff != 0 && w.cutoff < cutoff
}

// MarshalBinary serialized window block into binary data
func (w winBlock) MarshalBinary() []byte {
	buf := make([]byte, blockSize)
	data := buf
	for i := 0; i < seqsPerWindowBlock; i++ {
		e := w.entries[i]
		binary.LittleEndian.PutUint64(buf[:8], e.seq)
		binary.LittleEndian.PutUint32(buf[8:12], e.expiresAt)
		buf = buf[12:]
	}
	binary.LittleEndian.PutUint64(buf[:8], uint64(w.cutoff))
	binary.LittleEndian.PutUint64(buf[8:16], w.topicHash)
	binary.LittleEndian.PutUint64(buf[16:24], uint64(w.next))
	binary.LittleEndian.PutUint16(buf[24:26], w.entryIdx)
	return data
}

// UnmarshalBinary de-serialized window block from binary data
func (w *winBlock) UnmarshalBinary(data []byte) error {
	for i := 0; i < seqsPerWindowBlock; i++ {
		_ = data[12] // bounds check hint to compiler; see golang.org/issue/14808
		w.entries[i].seq = binary.LittleEndian.Uint64(data[:8])
		w.entries[i].expiresAt = binary.LittleEndian.Uint32(data[8:12])
		data = data[12:]
	}
	w.cutoff = int64(binary.LittleEndian.Uint64(data[:8]))
	w.topicHash = binary.LittleEndian.Uint64(data[8:16])
	w.next = int64(binary.LittleEndian.Uint64(data[16:24]))
	w.entryIdx = binary.LittleEndian.Uint16(data[24:26])
	return nil
}

type windowHandle struct {
	winBlock
	file
	offset int64
}

func winBlockOffset(idx int32) int64 {
	return (int64(blockSize) * int64(idx))
}

func (wh *windowHandle) read() error {
	buf, err := wh.file.Slice(wh.offset, wh.offset+int64(blockSize))
	if err != nil {
		return err
	}
	return wh.UnmarshalBinary(buf)
}

type windowEntries []winEntry
type timeWindow struct {
	mu sync.RWMutex

	freezed        bool
	entries        map[uint64]windowEntries // map[topicHash]windowEntries
	friezedEntries map[uint64]windowEntries
}

// A "thread" safe windows.
// To avoid lock bottlenecks windows are divided into several shards (nShards).
type windows struct {
	sync.RWMutex
	window     []*timeWindow
	consistent *hash.Consistent
}

// newWindows creates a new concurrent windows.
func newWindows() *windows {
	w := &windows{
		window:     make([]*timeWindow, nShards),
		consistent: hash.InitConsistent(int(nShards), int(nShards)),
	}

	for i := 0; i < nShards; i++ {
		w.window[i] = &timeWindow{friezedEntries: make(map[uint64]windowEntries), entries: make(map[uint64]windowEntries)}
	}

	return w
}

// getWindow returns shard under given key
func (w *windows) getWindow(key uint64) *timeWindow {
	w.RLock()
	defer w.RUnlock()
	return w.window[w.consistent.FindBlock(key)]
}

type (
	timeOptions struct {
		expDurationType     time.Duration
		maxExpDurations     int
		backgroundKeyExpiry bool
	}
	timeWindowBucket struct {
		sync.RWMutex
		file
		*windows
		*expiryWindowBucket
		windowIdx int32
		opts      *timeOptions
	}
)

func (src *timeOptions) copyWithDefaults() *timeOptions {
	opts := timeOptions{}
	if src != nil {
		opts = *src
	}
	if opts.expDurationType == 0 {
		opts.expDurationType = time.Minute
	}
	if opts.maxExpDurations == 0 {
		opts.maxExpDurations = 1
	}
	return &opts
}

func newTimeWindowBucket(f file, opts *timeOptions) *timeWindowBucket {
	l := &timeWindowBucket{file: f, windowIdx: -1}
	l.windows = newWindows()
	l.expiryWindowBucket = newExpiryWindowBucket(opts.backgroundKeyExpiry, opts.expDurationType, opts.maxExpDurations)
	l.opts = opts.copyWithDefaults()
	return l
}

type windowWriter struct {
	*timeWindowBucket
	winBlocks map[int32]winBlock // map[windowIdx]winBlock

	buffer *bpool.Buffer

	leasing map[int32][]uint64 // map[blockIdx][]seq
}

func newWindowWriter(wb *timeWindowBucket, buf *bpool.Buffer) *windowWriter {
	return &windowWriter{winBlocks: make(map[int32]winBlock), timeWindowBucket: wb, buffer: buf, leasing: make(map[int32][]uint64)}
}

func (wb *timeWindowBucket) add(topicHash uint64, e winEntry) error {
	// get windows shard
	ws := wb.getWindow(topicHash)
	ws.mu.Lock()
	defer ws.mu.Unlock()

	if ws.freezed {
		if _, ok := ws.friezedEntries[topicHash]; ok {
			ws.friezedEntries[topicHash] = append(ws.friezedEntries[topicHash], e)
		} else {
			ws.friezedEntries[topicHash] = windowEntries{e}
		}
		return nil
	}
	if _, ok := ws.entries[topicHash]; ok {
		ws.entries[topicHash] = append(ws.entries[topicHash], e)
	} else {
		ws.entries[topicHash] = windowEntries{e}
	}
	return nil
}

func (w *timeWindow) reset() error {
	w.entries = make(map[uint64]windowEntries)
	return nil
}

func (w *timeWindow) freeze() error {
	w.freezed = true
	return nil
}

func (w *timeWindow) unFreeze() error {
	w.freezed = false
	for h := range w.friezedEntries {
		w.entries[h] = append(w.entries[h], w.friezedEntries[h]...)
	}
	w.friezedEntries = make(map[uint64]windowEntries)
	return nil
}

// foreachTimeWindow iterates timewindow entries during sync or recovery process when writing entries to window file
func (wb *timeWindowBucket) foreachTimeWindow(freeze bool, f func(w map[uint64]windowEntries) (bool, error)) (err error) {
	for i := 0; i < nShards; i++ {
		ws := wb.windows.window[i]
		ws.mu.RLock()
		wEntries := make(map[uint64]windowEntries)
		if freeze {
			ws.freeze()
		}
		for h, entries := range ws.entries {
			wEntries[h] = entries
		}
		ws.mu.RUnlock()
		stop, err1 := f(wEntries)
		if stop || err1 != nil {
			err = err1
			if freeze {
				ws.mu.Lock()
				ws.unFreeze()
				ws.mu.Unlock()
			}
			continue
		}
		if freeze {
			ws.mu.Lock()
			ws.reset()
			ws.unFreeze()
			ws.mu.Unlock()
		}
	}
	return err
}

// foreachWindowBlock iterates winBlocks on DB init to store topic hash and last offset of topic into trie.
func (wb *timeWindowBucket) foreachWindowBlock(f func(windowHandle) (bool, error)) (err error) {
	winBlockIdx := int32(0)
	nWinBlocks := wb.windowIndex()
	for winBlockIdx < nWinBlocks {
		off := winBlockOffset(winBlockIdx)
		b := windowHandle{file: wb.file, offset: off}
		if err := b.read(); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if stop, err := f(b); stop || err != nil {
			return err
		}
		winBlockIdx++
	}
	return nil
}

// ilookup lookups window entries from timeWindowBucket and not yet sync to DB
func (wb *timeWindowBucket) ilookup(topicHash uint64, limit int) (winEntries windowEntries) {
	winEntries = make([]winEntry, 0)
	// get windows shard
	ws := wb.getWindow(topicHash)
	ws.mu.RLock()
	defer ws.mu.RUnlock()
	var l int
	var expiryCount int
	wEntries := ws.friezedEntries[topicHash]
	if len(wEntries) > 0 {
		l = limit
		if len(wEntries) < limit {
			l = len(wEntries)
		}
		for _, we := range wEntries[len(wEntries)-l:] { // most recent entries are appended to the end so get the entries from end
			if we.isExpired() {
				expiryCount++
				if err := wb.addExpiry(we); err != nil {
					logger.Error().Err(err).Str("context", "timeWindow.addExpiry")
				}
				// if id is expired it does not return an error but continue the iteration
				continue
			}
			winEntries = append(winEntries, we)
		}
	}
	wEntries = ws.entries[topicHash]
	if len(wEntries) > 0 {
		l = limit + expiryCount - l
		if len(wEntries) < l {
			l = len(wEntries)
		}
		for _, we := range wEntries[len(wEntries)-l:] {
			if we.isExpired() {
				if err := wb.addExpiry(we); err != nil {
					expiryCount++
					logger.Error().Err(err).Str("context", "timeWindow.addExpiry")
				}
				// if id is expired it does not return an error but continue the iteration
				continue
			}
			winEntries = append(winEntries, we)
		}
	}
	return winEntries
}

// lookup lookups window entries from window file.
func (wb *timeWindowBucket) lookup(topicHash uint64, off, cutoff int64, skip, limit int) (winEntries windowEntries) {
	// winEntries = make([]winEntry, 0)
	winEntries = wb.ilookup(topicHash, limit)
	if len(winEntries) >= limit {
		return winEntries
	}
	next := func(off int64, f func(windowHandle) (bool, error)) error {
		for {
			b := windowHandle{file: wb.file, offset: off}
			if err := b.read(); err != nil {
				return err
			}
			if stop, err := f(b); stop || err != nil {
				return err
			}
			if b.next == 0 {
				return nil
			}
			off = b.next
		}
	}
	count := 0
	expiryCount := 0
	err := next(off, func(curb windowHandle) (bool, error) {
		b := &curb
		if b.topicHash != topicHash {
			return true, nil
		}
		if skip > count+int(b.entryIdx) {
			count += int(b.entryIdx)
			return false, nil
		}
		if len(winEntries) > limit-int(b.entryIdx) {
			limit = limit - len(winEntries)
			for _, we := range b.entries[b.entryIdx-uint16(limit) : b.entryIdx] {
				if we.isExpired() {
					if err := wb.addExpiry(we); err != nil {
						expiryCount++
						logger.Error().Err(err).Str("context", "timeWindow.addExpiry")
					}
					// if id is expired it does not return an error but continue the iteration
					continue
				}
				winEntries = append(winEntries, we)
			}
			if len(winEntries) >= limit {
				return true, nil
			}
		}
		for _, we := range b.entries[:b.entryIdx] {
			if we.isExpired() {
				if err := wb.addExpiry(we); err != nil {
					expiryCount++
					logger.Error().Err(err).Str("context", "timeWindow.addExpiry")
				}
				// if id is expired it does not return an error but continue the iteration
				continue
			}
			winEntries = append(winEntries, we)

		}
		if b.Cutoff(cutoff) {
			return true, nil
		}
		return false, nil
	})
	if err != nil {
		return winEntries
	}

	return winEntries
}

func (w winBlock) validation(topicHash uint64) error {
	if w.topicHash != topicHash {
		return fmt.Errorf("timeWindow.write: validation failed block topicHash %d, topicHash %d", w.topicHash, topicHash)
	}
	return nil
}

func (wb *windowWriter) del(seq uint64, bIdx int32) error {
	off := int64(blockSize * uint32(bIdx))
	w := windowHandle{file: wb.file, offset: off}
	if err := w.read(); err != nil {
		return err
	}
	entryIdx := -1
	for i := 0; i < int(w.entryIdx); i++ {
		e := w.entries[i]
		if e.seq == seq { //record exist in db
			entryIdx = i
			break
		}
	}
	if entryIdx == -1 {
		return nil // no entry in db to delete
	}
	w.entryIdx--

	i := entryIdx
	for ; i < entriesPerIndexBlock-1; i++ {
		w.entries[i] = w.entries[i+1]
	}
	w.entries[i] = winEntry{}

	return nil
}

// append appends window entries to buffer
func (wb *windowWriter) append(topicHash uint64, off int64, wEntries windowEntries) (newOff int64, err error) {
	var w winBlock
	var ok bool
	var winIdx int32
	if off == 0 {
		wb.windowIdx++
		winIdx = wb.windowIdx
	} else {
		winIdx = int32(off / int64(blockSize))
	}
	w, ok = wb.winBlocks[winIdx]
	if !ok && off > 0 {
		if winIdx <= wb.windowIdx {
			wh := windowHandle{file: wb.file, offset: off}
			if err := wh.read(); err != nil && err != io.EOF {
				return off, err
			}
			w = wh.winBlock
			w.validation(topicHash)
			w.leased = true
		}
	}
	w.topicHash = topicHash
	for _, we := range wEntries {
		if we.seq == 0 {
			continue
		}
		entryIdx := 0
		for i := 0; i < seqsPerWindowBlock; i++ {
			e := w.entries[i]
			if e.seq == we.seq { //record exist in db
				entryIdx = -1
				break
			}
		}
		if entryIdx == -1 {
			continue
		}
		if w.entryIdx == seqsPerWindowBlock {
			topicHash := w.topicHash
			next := int64(blockSize * uint32(winIdx))
			// set approximate cutoff on winBlock
			w.cutoff = time.Now().Unix()
			wb.winBlocks[winIdx] = w
			wb.windowIdx++
			winIdx = wb.windowIdx
			w = winBlock{topicHash: topicHash, next: next}
		}
		if w.leased {
			wb.leasing[winIdx] = append(wb.leasing[winIdx], we.seq)
		}
		w.entries[w.entryIdx] = winEntry{seq: we.seq, expiresAt: we.expiresAt}
		w.dirty = true
		w.entryIdx++
	}

	wb.winBlocks[winIdx] = w
	return int64(blockSize * uint32(winIdx)), nil
}

func (wb *windowWriter) write() error {
	for bIdx, w := range wb.winBlocks {
		if !w.leased || !w.dirty {
			continue
		}
		off := int64(blockSize * uint32(bIdx))
		if _, err := wb.WriteAt(w.MarshalBinary(), off); err != nil {
			return err
		}
		w.dirty = false
		wb.winBlocks[bIdx] = w
	}

	// sort blocks by blockIdx
	var blockIdx []int
	for bIdx := range wb.winBlocks {
		if wb.winBlocks[bIdx].leased || !wb.winBlocks[bIdx].dirty {
			continue
		}
		blockIdx = append(blockIdx, int(bIdx))
	}
	sort.Ints(blockIdx)

	winBlocks, err := blockRange(blockIdx)
	if err != nil {
		return err
	}
	bufOff := int64(0)
	for _, blocks := range winBlocks {
		if len(blocks) == 1 {
			bIdx := int32(blocks[0])
			off := int64(blockSize * uint32(bIdx))
			w := wb.winBlocks[bIdx]
			buf := w.MarshalBinary()
			if _, err := wb.WriteAt(buf, off); err != nil {
				return err
			}
			w.dirty = false
			wb.winBlocks[bIdx] = w
			continue
		}
		blockOff := int64(blockSize * uint32(blocks[0]))
		for bIdx := int32(blocks[0]); bIdx <= int32(blocks[1]); bIdx++ {
			w := wb.winBlocks[bIdx]
			wb.buffer.Write(w.MarshalBinary())
			w.dirty = false
			wb.winBlocks[bIdx] = w
		}
		blockData, err := wb.buffer.Slice(bufOff, wb.buffer.Size())
		if err != nil {
			return err
		}
		if _, err := wb.WriteAt(blockData, blockOff); err != nil {
			return err
		}
		bufOff = wb.buffer.Size()
	}
	return nil
}

func (wb *windowWriter) rollback() error {
	for bIdx, seqs := range wb.leasing {
		for _, seq := range seqs {
			if err := wb.del(seq, bIdx); err != nil {
				return err
			}
		}
	}
	return nil
}

func (wb *timeWindowBucket) windowIndex() int32 {
	return wb.windowIdx
}

func (wb *timeWindowBucket) setWindowIndex(windowIdx int32) error {
	wb.windowIdx = windowIdx
	return nil
}
