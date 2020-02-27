package tracedb

import (
	"sync"

	"github.com/unit-io/tracedb/message"
)

const (
	nul = 0x0
)

// winEntries represents a time entry set which can contain only unique values.
type winEntries []winEntry

// new returns time entry set of given cap.
func newWinEntries(cap uint32) winEntries {
	return make([]winEntry, 0, cap)
}

// extend extends the cap of time entry set.
func (ts *winEntries) extend(cap uint32) {
	if cap < ts.len() {
		return
	}
	l := cap - ts.len()
	*ts = append(*ts, make([]winEntry, l)...)
}

// shrink shrinks the cap of seq set.
func (ts *winEntries) shrink(cap uint32) {
	newts := make([]winEntry, 0, ts.len())
	copy(newts, *ts)
	*ts = newts
}

// addUnique adds a seq to the set.
func (ts *winEntries) addUnique(value winEntry) (added bool) {
	if ts.contains(value) == false {
		*ts = append(*ts, value)
		added = true
	}
	return
}

// remove a seq from the set.
func (ts *winEntries) remove(value winEntry) (removed bool) {
	for i, v := range *ts {
		// if bytes.Equal(v, value) {
		if v == value {
			a := *ts
			a[i] = a[len(a)-1]
			//a[len(a)-1] = nil
			a = a[:len(a)-1]
			*ts = a
			removed = true
			return
		}
	}
	return
}

// contains checks whether a seq is in the set.
func (ts *winEntries) contains(value winEntry) bool {
	for _, v := range *ts {
		// if bytes.Equal(v, value) {
		// 	return true
		// }
		if v == value {
			return true
		}
	}
	return false
}

// len length of seq set.
func (ts *winEntries) len() uint32 {
	return uint32(len(*ts))
}

type key struct {
	query     uint32
	wildchars uint8
}

type part struct {
	k         key
	depth     uint8
	cap       uint32
	ts        winEntries
	parent    *part
	children  map[key]*part
	offset    int64
	topicHash uint64
}

func (p *part) orphan() {
	if p.parent == nil {
		return
	}

	delete(p.parent.children, p.k)
	if len(p.parent.ts) == 0 && len(p.parent.children) == 0 {
		p.parent.orphan()
	}
}

// partTrie represents an efficient collection of Trie with lookup capability.
type partTrie struct {
	summary map[uint64]*part // summary is map of topichash to part
	root    *part            // The root node of the tree.
}

// newPartTrie creates a new matcher for the Trie.
func newPartTrie(cacheCap uint32) *partTrie {
	return &partTrie{
		summary: make(map[uint64]*part),
		root: &part{
			cap:      cacheCap,
			ts:       newWinEntries(cacheCap),
			children: make(map[key]*part),
		},
	}
}

// trie trie data structure to store topic parts
type trie struct {
	sync.RWMutex
	mutex
	partTrie *partTrie
}

// NewTrie new trie creates a Trie with an initialized Trie.
// Mutex is used to lock concurent read/write on a contract, and it does not lock entire trie.
func newTrie(cacheCap uint32) *trie {
	return &trie{
		mutex:    newMutex(),
		partTrie: newPartTrie(cacheCap),
	}
}

// Count returns the number of topics in the Trie.
func (t *trie) Count() int {
	t.RLock()
	defer t.RUnlock()
	return len(t.partTrie.summary)
}

// add adds a message seq to topic trie.
func (t *trie) addTopic(contract uint64, topicHash uint64, parts []message.Part, depth uint8) (added bool) {
	// Get mutex
	mu := t.getMutex(contract)
	mu.Lock()
	defer mu.Unlock()
	if _, ok := t.partTrie.summary[topicHash]; ok {
		return true
	}
	curr := t.partTrie.root
	for _, p := range parts {
		k := key{
			query:     p.Query,
			wildchars: p.Wildchars,
		}
		t.RLock()
		child, ok := curr.children[k]
		t.RUnlock()
		if !ok {
			child = &part{
				k:         k,
				cap:       t.partTrie.root.cap,
				ts:        newWinEntries(t.partTrie.root.cap),
				parent:    curr,
				children:  make(map[key]*part),
				topicHash: topicHash,
			}
			t.Lock()
			curr.children[k] = child
			t.Unlock()
		}
		curr = child
	}
	t.Lock()
	t.partTrie.summary[topicHash] = curr
	t.Unlock()
	added = true
	curr.depth = depth
	return
}

// add adds a message seq to topic trie.
func (t *trie) add(topicHash uint64, we winEntry) (added bool) {
	// Get mutex
	mu := t.getMutex(we.contract)
	mu.Lock()
	defer mu.Unlock()
	t.RLock()
	curr, ok := t.partTrie.summary[topicHash]
	t.RUnlock()
	if !ok {
		return false
	}
	if curr.ts.len() >= curr.cap {
		curr.ts = curr.ts[1:] // remove first if capacity has reached
	}
	curr.ts = append(curr.ts, we)
	added = true
	return
}

// remove removes a message seq from topic trie
func (t *trie) remove(topicHash uint64, we winEntry) (removed bool) {
	mu := t.getMutex(we.contract)
	mu.Lock()
	defer mu.Unlock()
	t.RLock()
	curr, ok := t.partTrie.summary[topicHash]
	t.RUnlock()
	if !ok {
		return false
	}
	// Remove a message seq and decrement the counter
	if ok := curr.ts.remove(we); ok {
		removed = true
		// adjust cap of the seq set
		if curr.ts.len() > t.partTrie.root.cap {
			curr.cap = curr.ts.len()
			curr.ts.shrink(curr.cap)
		}
	}
	// Remove orphans
	t.Lock()
	defer t.Unlock()
	if len(curr.ts) == 0 && len(curr.children) == 0 {
		curr.orphan()
	}
	return
}

// lookup returns seq set for given topic.
func (t *trie) lookup(contract uint64, parts []message.Part, limit uint32) (tss []winEntries, topicHss []uint64, offs []int64) {
	t.RLock()
	mu := t.getMutex(contract)
	mu.Lock()
	defer func() {
		t.RUnlock()
		mu.Unlock()
	}()

	t.ilookup(contract, parts, uint8(len(parts)-1), &tss, &topicHss, &offs, t.partTrie.root, limit)
	return tss, topicHss, offs
}

func (t *trie) ilookup(contract uint64, parts []message.Part, depth uint8, tss *[]winEntries, topicHss *[]uint64, offs *[]int64, part *part, limit uint32) {
	l := limit
	// Add seq set from the current branch
	if part.depth == depth || (part.depth >= message.TopicMaxDepth && depth > part.depth-message.TopicMaxDepth) {
		*topicHss = append(*topicHss, part.topicHash)
		*offs = append(*offs, part.offset)
		if part.ts.len() > 0 {
			if uint32(part.ts.len()) < l {
				l = uint32(part.ts.len())
			}
			*tss = append(*tss, part.ts[uint32(part.ts.len())-l:]) // begin from end to get recent entries
			// set new limit
			l = limit - l
			// on lookup cap increased to 2 folds of current cap of the set
			if part.ts.len() > limit {
				if part.cap < 2*limit {
					part.cap = 2 * limit
					part.ts.extend(part.cap)
				}
			}
		}
	}

	// If we're not yet done, continue
	if len(parts) > 0 {
		// Go through the exact match branch
		for k, p := range part.children {
			if k.query == parts[0].Query && uint8(len(parts)) >= k.wildchars+1 {
				t.ilookup(contract, parts[k.wildchars+1:], depth, tss, topicHss, offs, p, l)
			}
		}
	}
}

func (t *trie) getOffset(topicHash uint64) (off int64, ok bool) {
	t.RLock()
	defer t.RUnlock()
	if curr, ok := t.partTrie.summary[topicHash]; ok {
		return curr.offset, ok
	}
	return 0, ok
}

func (t *trie) setOffset(topicHash uint64, off int64) (ok bool) {
	t.Lock()
	defer t.Unlock()
	if curr, ok := t.partTrie.summary[topicHash]; ok {
		if curr.offset < off {
			curr.offset = off
		}
		return ok
	}
	return false
}
