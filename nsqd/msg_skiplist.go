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

// msgSkipListNode represents a node in the skiplist
type msgSkipListNode struct {
	message *Message
	key     int64 // timestamp (msg.pri field)
	forward []*msgSkipListNode
}

// msgSkipList is a skiplist for timeout-ordered message processing
type msgSkipList struct {
	header *msgSkipListNode
	level  int
	length int
	mu     sync.RWMutex
	rand   *rand.Rand
}

func newMsgSkipList() *msgSkipList {
	header := &msgSkipListNode{
		forward: make([]*msgSkipListNode, maxSkipListLevel),
	}
	return &msgSkipList{
		header: header,
		level:  0,
		length: 0,
		rand:   rand.New(rand.NewSource(time.Now().UnixNano())),
	}
}

func (sl *msgSkipList) randomLevel() int {
	level := 0
	for level < maxSkipListLevel-1 && sl.rand.Float32() < skipListP {
		level++
	}
	return level
}

// Insert adds a message to the skiplist ordered by pri field
func (sl *msgSkipList) Insert(msg *Message) {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	key := msg.pri
	update := make([]*msgSkipListNode, maxSkipListLevel)
	current := sl.header

	for i := sl.level; i >= 0; i-- {
		for current.forward[i] != nil && current.forward[i].key < key {
			current = current.forward[i]
		}
		update[i] = current
	}

	newLevel := sl.randomLevel()
	if newLevel > sl.level {
		for i := sl.level + 1; i <= newLevel; i++ {
			update[i] = sl.header
		}
		sl.level = newLevel
	}

	newNode := &msgSkipListNode{
		message: msg,
		key:     key,
		forward: make([]*msgSkipListNode, newLevel+1),
	}

	for i := 0; i <= newLevel; i++ {
		newNode.forward[i] = update[i].forward[i]
		update[i].forward[i] = newNode
	}

	sl.length++
}

// Remove removes a message from the skiplist by key and ID
func (sl *msgSkipList) Remove(msg *Message) bool {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	key := msg.pri
	update := make([]*msgSkipListNode, maxSkipListLevel)
	current := sl.header

	for i := sl.level; i >= 0; i-- {
		for current.forward[i] != nil && current.forward[i].key < key {
			current = current.forward[i]
		}
		update[i] = current
	}

	current = current.forward[0]

	if current != nil && current.key == key && current.message.ID == msg.ID {
		for i := 0; i <= sl.level; i++ {
			if update[i].forward[i] != current {
				break
			}
			update[i].forward[i] = current.forward[i]
		}

		for sl.level > 0 && sl.header.forward[sl.level] == nil {
			sl.level--
		}

		sl.length--
		return true
	}

	return false
}

// PeekAndShift returns the message with the earliest timestamp if it's <= max, removing it
func (sl *msgSkipList) PeekAndShift(max int64) (*Message, int64) {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	if sl.length == 0 {
		return nil, 0
	}

	first := sl.header.forward[0]
	if first == nil || first.key > max {
		if first != nil {
			return nil, first.key - max
		}
		return nil, 0
	}

	msg := first.message

	for i := 0; i <= sl.level; i++ {
		if sl.header.forward[i] == first {
			sl.header.forward[i] = first.forward[i]
		} else {
			break
		}
	}

	for sl.level > 0 && sl.header.forward[sl.level] == nil {
		sl.level--
	}

	sl.length--
	return msg, 0
}

// Len returns the number of messages in the skiplist
func (sl *msgSkipList) Len() int {
	sl.mu.RLock()
	defer sl.mu.RUnlock()
	return sl.length
}

// Clear removes all messages from the skiplist
func (sl *msgSkipList) Clear() {
	sl.mu.Lock()
	defer sl.mu.Unlock()

	sl.header.forward = make([]*msgSkipListNode, maxSkipListLevel)
	sl.level = 0
	sl.length = 0
}
