package nsqd

import (
	"math/rand"
	"sync"
	"time"
)

const (
	maxSkipListLevel = 32
	skipListP        = 0.25 // probability for level increase
)

// skipListNode represents a node in the skiplist
type skipListNode struct {
	message *Message
	key     int64 // timeout timestamp (pri field)
	forward []*skipListNode
}

// inFlightSkipList is a concurrent skiplist optimized for timeout-ordered message processing
type inFlightSkipList struct {
	header *skipListNode
	level  int
	length int
	mu     sync.RWMutex
	rand   *rand.Rand
}

// newInFlightSkipList creates a new skiplist for in-flight message timeouts
func newInFlightSkipList() *inFlightSkipList {
	header := &skipListNode{
		forward: make([]*skipListNode, maxSkipListLevel),
	}
	return &inFlightSkipList{
		header: header,
		level:  0,
		length: 0,
		rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

// randomLevel generates a random level for a new node
func (sl *inFlightSkipList) randomLevel() int {
	level := 0
	for level < maxSkipListLevel-1 && sl.rand.Float32() < skipListP {
		level++
	}
	return level
}

// Insert adds a message to the skiplist ordered by timeout (pri field)
func (sl *inFlightSkipList) Insert(msg *Message) {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	key := msg.pri
	update := make([]*skipListNode, maxSkipListLevel)
	current := sl.header

	// Find position to insert
	for i := sl.level; i >= 0; i-- {
		for current.forward[i] != nil && current.forward[i].key < key {
			current = current.forward[i]
		}
		update[i] = current
	}

	// Generate random level for new node
	newLevel := sl.randomLevel()
	if newLevel > sl.level {
		for i := sl.level + 1; i <= newLevel; i++ {
			update[i] = sl.header
		}
		sl.level = newLevel
	}

	// Create new node
	newNode := &skipListNode{
		message: msg,
		key:     key,
		forward: make([]*skipListNode, newLevel+1),
	}

	// Update forward pointers
	for i := 0; i <= newLevel; i++ {
		newNode.forward[i] = update[i].forward[i]
		update[i].forward[i] = newNode
	}

	sl.length++
}

// Remove removes a message from the skiplist by finding it via message ID
func (sl *inFlightSkipList) Remove(msg *Message) bool {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	key := msg.pri
	update := make([]*skipListNode, maxSkipListLevel)
	current := sl.header

	// Find the node to remove
	for i := sl.level; i >= 0; i-- {
		for current.forward[i] != nil && current.forward[i].key < key {
			current = current.forward[i]
		}
		update[i] = current
	}

	current = current.forward[0]
	
	// Check if we found the right message (same ID)
	if current != nil && current.key == key && current.message.ID == msg.ID {
		// Remove the node
		for i := 0; i <= sl.level; i++ {
			if update[i].forward[i] != current {
				break
			}
			update[i].forward[i] = current.forward[i]
		}

		// Update level if necessary
		for sl.level > 0 && sl.header.forward[sl.level] == nil {
			sl.level--
		}

		sl.length--
		return true
	}

	return false
}

// PeekAndShift returns the message with earliest timeout if it's <= max, removing it from skiplist
func (sl *inFlightSkipList) PeekAndShift(max int64) (*Message, int64) {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	if sl.length == 0 {
		return nil, 0
	}

	// Check the first (minimum) element
	first := sl.header.forward[0]
	if first == nil || first.key > max {
		if first != nil {
			return nil, first.key - max
		}
		return nil, 0
	}

	// Remove the first element
	msg := first.message
	
	// Update forward pointers
	for i := 0; i <= sl.level; i++ {
		if sl.header.forward[i] == first {
			sl.header.forward[i] = first.forward[i]
		} else {
			break
		}
	}

	// Update level if necessary
	for sl.level > 0 && sl.header.forward[sl.level] == nil {
		sl.level--
	}

	sl.length--
	return msg, 0
}

// Len returns the number of messages in the skiplist
func (sl *inFlightSkipList) Len() int {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.length
}

// Clear removes all messages from the skiplist
func (sl *inFlightSkipList) Clear() {
	sl.mu.Lock()
	defer sl.mu.Unlock()
	
	sl.header.forward = make([]*skipListNode, maxSkipListLevel)
	sl.level = 0
	sl.length = 0
}