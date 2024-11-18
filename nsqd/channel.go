package nsqd

import (
	"errors"
	"fmt"
	"math"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/nsqio/go-diskqueue"

	"github.com/nsqio/nsq/internal/lg"
	"github.com/nsqio/nsq/internal/pqueue"
	"github.com/nsqio/nsq/internal/quantile"
)

type Consumer interface {
	UnPause()
	Pause()
	Close() error
	TimedOutMessage()
	Stats(string) ClientStats
	Empty()
}

// Channel represents the concrete type for a NSQ channel (and also
// implements the Queue interface)
//
// There can be multiple channels per topic, each with there own unique set
// of subscribers (clients).
//
// Channels maintain all client and message metadata, orchestrating in-flight
// messages, timeouts, requeuing, etc.
type Channel struct {
	// 64bit atomic vars need to be first for proper alignment on 32bit platforms
	requeueCount uint64
	messageCount uint64
	timeoutCount uint64

	sync.RWMutex

	topicName string
	name      string
	nsqd      *NSQD

	backend BackendQueue

	memoryMsgChan chan *Message
	exitFlag      int32
	exitMutex     sync.RWMutex

	// state tracking
	clients        map[int64]Consumer
	paused         int32
	ephemeral      bool
	deleteCallback func(*Channel)
	deleter        sync.Once

	// Stats tracking
	e2eProcessingLatencyStream *quantile.Quantile

	// TODO: these can be DRYd up
	deferredMessages map[MessageID]*pqueue.Item[*Message, int64]
	deferredPQ       *pqueue.PriorityQueue[*Message, int64]
	deferredMutex    sync.Mutex
	inFlightMessages map[MessageID]*pqueue.Item[*Message, int64]
	inFlightPQ       *pqueue.PriorityQueue[*Message, int64]
	inFlightMutex    sync.Mutex
}

// NewChannel creates a new instance of the Channel type and returns a pointer
func NewChannel(topicName string, channelName string, nsqd *NSQD,
	deleteCallback func(*Channel)) *Channel {

	c := &Channel{
		topicName:      topicName,
		name:           channelName,
		memoryMsgChan:  nil,
		clients:        make(map[int64]Consumer),
		deleteCallback: deleteCallback,
		nsqd:           nsqd,
		ephemeral:      strings.HasSuffix(channelName, "#ephemeral"),
	}
	// avoid mem-queue if size == 0 for more consistent ordering
	if nsqd.getOpts().MemQueueSize > 0 || c.ephemeral {
		c.memoryMsgChan = make(chan *Message, nsqd.getOpts().MemQueueSize)
	}
	if len(nsqd.getOpts().E2EProcessingLatencyPercentiles) > 0 {
		c.e2eProcessingLatencyStream = quantile.New(
			nsqd.getOpts().E2EProcessingLatencyWindowTime,
			nsqd.getOpts().E2EProcessingLatencyPercentiles,
		)
	}

	c.initPQ()

	if c.ephemeral {
		c.backend = newDummyBackendQueue()
	} else {
		dqLogf := func(level diskqueue.LogLevel, f string, args ...interface{}) {
			opts := nsqd.getOpts()
			lg.Logf(opts.Logger, opts.LogLevel, lg.LogLevel(level), f, args...)
		}
		// backend names, for uniqueness, automatically include the topic...
		backendName := getBackendName(topicName, channelName)
		c.backend = diskqueue.New(
			backendName,
			nsqd.getOpts().DataPath,
			nsqd.getOpts().MaxBytesPerFile,
			int32(minValidMsgLength),
			int32(nsqd.getOpts().MaxMsgSize)+minValidMsgLength,
			nsqd.getOpts().SyncEvery,
			nsqd.getOpts().SyncTimeout,
			dqLogf,
		)
	}

	c.nsqd.Notify(c, !c.ephemeral)

	return c
}

func (c *Channel) initPQ() {
	pqSize := int(math.Max(1, float64(c.nsqd.getOpts().MemQueueSize)/10))

	c.inFlightMutex.Lock()
	c.inFlightMessages = make(map[MessageID]*pqueue.Item[*Message, int64])
	c.inFlightPQ = pqueue.New[*Message, int64](pqSize, pqueue.Min[int64])
	c.inFlightMutex.Unlock()

	c.deferredMutex.Lock()
	c.deferredMessages = make(map[MessageID]*pqueue.Item[*Message, int64])
	c.deferredPQ = pqueue.New[*Message, int64](pqSize, pqueue.Min[int64])
	c.deferredMutex.Unlock()
}

// Exiting returns a boolean indicating if this channel is closed/exiting
func (c *Channel) Exiting() bool {
	return atomic.LoadInt32(&c.exitFlag) == 1
}

// Delete empties the channel and closes
func (c *Channel) Delete() error {
	return c.exit(true)
}

// Close cleanly closes the Channel
func (c *Channel) Close() error {
	return c.exit(false)
}

func (c *Channel) exit(deleted bool) error {
	c.exitMutex.Lock()
	defer c.exitMutex.Unlock()

	if !atomic.CompareAndSwapInt32(&c.exitFlag, 0, 1) {
		return errors.New("exiting")
	}

	if deleted {
		c.nsqd.logf(LOG_INFO, "CHANNEL(%s): deleting", c.name)

		// since we are explicitly deleting a channel (not just at system exit time)
		// de-register this from the lookupd
		c.nsqd.Notify(c, !c.ephemeral)
	} else {
		c.nsqd.logf(LOG_INFO, "CHANNEL(%s): closing", c.name)
	}

	// this forceably closes client connections
	c.RLock()
	for _, client := range c.clients {
		client.Close()
	}
	c.RUnlock()

	if deleted {
		// empty the queue (deletes the backend files, too)
		c.Empty()
		return c.backend.Delete()
	}

	// write anything leftover to disk
	c.flush()
	return c.backend.Close()
}

func (c *Channel) Empty() error {
	c.Lock()
	defer c.Unlock()

	c.initPQ()
	for _, client := range c.clients {
		client.Empty()
	}

	for {
		select {
		case <-c.memoryMsgChan:
		default:
			goto finish
		}
	}

finish:
	return c.backend.Empty()
}

// flush persists all the messages in internal memory buffers to the backend
// it does not drain inflight/deferred because it is only called in Close()
func (c *Channel) flush() error {
	if len(c.memoryMsgChan) > 0 || len(c.inFlightMessages) > 0 || len(c.deferredMessages) > 0 {
		c.nsqd.logf(LOG_INFO, "CHANNEL(%s): flushing %d memory %d in-flight %d deferred messages to backend",
			c.name, len(c.memoryMsgChan), len(c.inFlightMessages), len(c.deferredMessages))
	}

	for {
		select {
		case msg := <-c.memoryMsgChan:
			err := writeMessageToBackend(msg, c.backend)
			if err != nil {
				c.nsqd.logf(LOG_ERROR, "failed to write message to backend - %s", err)
			}
		default:
			goto finish
		}
	}

finish:
	c.inFlightMutex.Lock()
	for _, item := range c.inFlightMessages {
		err := writeMessageToBackend(item.Val, c.backend)
		if err != nil {
			c.nsqd.logf(LOG_ERROR, "failed to write message to backend - %s", err)
		}
	}
	c.inFlightMutex.Unlock()

	c.deferredMutex.Lock()
	for _, item := range c.deferredMessages {
		err := writeMessageToBackend(item.Val, c.backend)
		if err != nil {
			c.nsqd.logf(LOG_ERROR, "failed to write message to backend - %s", err)
		}
	}
	c.deferredMutex.Unlock()

	return nil
}

func (c *Channel) Depth() int64 {
	return int64(len(c.memoryMsgChan)) + c.backend.Depth()
}

func (c *Channel) Pause() error {
	return c.doPause(true)
}

func (c *Channel) UnPause() error {
	return c.doPause(false)
}

func (c *Channel) doPause(pause bool) error {
	if pause {
		atomic.StoreInt32(&c.paused, 1)
	} else {
		atomic.StoreInt32(&c.paused, 0)
	}

	c.RLock()
	for _, client := range c.clients {
		if pause {
			client.Pause()
		} else {
			client.UnPause()
		}
	}
	c.RUnlock()
	return nil
}

func (c *Channel) IsPaused() bool {
	return atomic.LoadInt32(&c.paused) == 1
}

// PutMessage writes a Message to the queue
func (c *Channel) PutMessage(m *Message) error {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()
	if c.Exiting() {
		return errors.New("exiting")
	}
	err := c.put(m)
	if err != nil {
		return err
	}
	atomic.AddUint64(&c.messageCount, 1)
	return nil
}

func (c *Channel) put(m *Message) error {
	select {
	case c.memoryMsgChan <- m:
	default:
		err := writeMessageToBackend(m, c.backend)
		c.nsqd.SetHealth(err)
		if err != nil {
			c.nsqd.logf(LOG_ERROR, "CHANNEL(%s): failed to write message to backend - %s",
				c.name, err)
			return err
		}
	}
	return nil
}

func (c *Channel) PutMessageDeferred(msg *Message, timeout time.Duration) {
	atomic.AddUint64(&c.messageCount, 1)
	c.StartDeferredTimeout(msg, timeout)
}

// TouchMessage resets the timeout for an in-flight message
func (c *Channel) TouchMessage(clientID int64, id MessageID, clientMsgTimeout time.Duration) error {
	item, err := c.popInFlightMessage(clientID, id, true)
	if err != nil {
		return err
	}

	newTimeout := time.Now().Add(clientMsgTimeout)
	if newTimeout.Sub(item.Val.deliveryTS) >=
		c.nsqd.getOpts().MaxMsgTimeout {
		// we would have gone over, set to the max
		newTimeout = item.Val.deliveryTS.Add(c.nsqd.getOpts().MaxMsgTimeout)
	}

	c.inFlightMutex.Lock()
	item.Priority = newTimeout.UnixNano()
	c.inFlightPQ.Update(item)
	c.inFlightMutex.Unlock()

	return nil
}

// FinishMessage successfully discards an in-flight message
func (c *Channel) FinishMessage(clientID int64, id MessageID) error {
	item, err := c.popInFlightMessage(clientID, id, false)
	if err != nil {
		return err
	}
	c.removeFromInFlightPQ(item)
	if c.e2eProcessingLatencyStream != nil {
		c.e2eProcessingLatencyStream.Insert(item.Val.Timestamp)
	}
	return nil
}

// RequeueMessage requeues a message based on `time.Duration`, ie:
//
// `timeoutMs` == 0 - requeue a message immediately
// `timeoutMs`  > 0 - asynchronously wait for the specified timeout
//
//	and requeue a message (aka "deferred requeue")
func (c *Channel) RequeueMessage(clientID int64, id MessageID, timeout time.Duration) error {
	// remove from inflight first
	item, err := c.popInFlightMessage(clientID, id, false)
	if err != nil {
		return err
	}
	c.removeFromInFlightPQ(item)
	atomic.AddUint64(&c.requeueCount, 1)

	if timeout == 0 {
		c.exitMutex.RLock()
		if c.Exiting() {
			c.exitMutex.RUnlock()
			return errors.New("exiting")
		}
		err := c.put(item.Val)
		c.exitMutex.RUnlock()
		return err
	}

	// deferred requeue
	return c.StartDeferredTimeout(item.Val, timeout)
}

// AddClient adds a client to the Channel's client list
func (c *Channel) AddClient(clientID int64, client Consumer) error {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return errors.New("exiting")
	}

	c.RLock()
	_, ok := c.clients[clientID]
	numClients := len(c.clients)
	c.RUnlock()
	if ok {
		return nil
	}

	maxChannelConsumers := c.nsqd.getOpts().MaxChannelConsumers
	if maxChannelConsumers != 0 && numClients >= maxChannelConsumers {
		return fmt.Errorf("consumers for %s:%s exceeds limit of %d",
			c.topicName, c.name, maxChannelConsumers)
	}

	c.Lock()
	c.clients[clientID] = client
	c.Unlock()
	return nil
}

// RemoveClient removes a client from the Channel's client list
func (c *Channel) RemoveClient(clientID int64) {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return
	}

	c.RLock()
	_, ok := c.clients[clientID]
	c.RUnlock()
	if !ok {
		return
	}

	c.Lock()
	delete(c.clients, clientID)
	numClients := len(c.clients)
	c.Unlock()

	if numClients == 0 && c.ephemeral {
		go c.deleter.Do(func() { c.deleteCallback(c) })
	}
}

func (c *Channel) StartInFlightTimeout(msg *Message, clientID int64, timeout time.Duration) error {
	now := time.Now()
	msg.clientID = clientID
	msg.deliveryTS = now
	item := &pqueue.Item[*Message, int64]{
		Val:      msg,
		Priority: now.Add(timeout).UnixNano(),
	}
	err := c.pushInFlightMessage(item)
	if err != nil {
		return err
	}
	c.addToInFlightPQ(item)
	return nil
}

func (c *Channel) StartDeferredTimeout(msg *Message, timeout time.Duration) error {
	item := &pqueue.Item[*Message, int64]{
		Val:      msg,
		Priority: time.Now().Add(timeout).UnixNano(),
	}
	err := c.pushDeferredMessage(item)
	if err != nil {
		return err
	}
	c.addToDeferredPQ(item)
	return nil
}

// pushInFlightMessage atomically adds a message to the in-flight dictionary
func (c *Channel) pushInFlightMessage(item *pqueue.Item[*Message, int64]) error {
	c.inFlightMutex.Lock()
	_, ok := c.inFlightMessages[item.Val.ID]
	if ok {
		c.inFlightMutex.Unlock()
		return errors.New("ID already in flight")
	}
	c.inFlightMessages[item.Val.ID] = item
	c.inFlightMutex.Unlock()
	return nil
}

// popInFlightMessage atomically removes a message from the in-flight dictionary
func (c *Channel) popInFlightMessage(clientID int64, id MessageID, peek bool) (*pqueue.Item[*Message, int64], error) {
	c.inFlightMutex.Lock()
	item, ok := c.inFlightMessages[id]
	if !ok {
		c.inFlightMutex.Unlock()
		return nil, errors.New("ID not in flight")
	}
	if item.Val.clientID != clientID {
		c.inFlightMutex.Unlock()
		return nil, errors.New("client does not own message")
	}
	if !peek {
		delete(c.inFlightMessages, id)
	}
	c.inFlightMutex.Unlock()
	return item, nil
}

func (c *Channel) addToInFlightPQ(item *pqueue.Item[*Message, int64]) {
	c.inFlightMutex.Lock()
	c.inFlightPQ.Push(item)
	c.inFlightMutex.Unlock()
}

func (c *Channel) removeFromInFlightPQ(item *pqueue.Item[*Message, int64]) {
	c.inFlightMutex.Lock()
	// has this item has already been popped off the pqueue?
	if item.Index != -1 {
		c.inFlightPQ.Remove(item.Index)
	}
	c.inFlightMutex.Unlock()
}

func (c *Channel) pushDeferredMessage(item *pqueue.Item[*Message, int64]) error {
	c.deferredMutex.Lock()
	// TODO: these map lookups are costly
	_, ok := c.deferredMessages[item.Val.ID]
	if ok {
		c.deferredMutex.Unlock()
		return errors.New("ID already deferred")
	}
	c.deferredMessages[item.Val.ID] = item
	c.deferredMutex.Unlock()
	return nil
}

func (c *Channel) popDeferredMessage(id MessageID) (*pqueue.Item[*Message, int64], error) {
	c.deferredMutex.Lock()
	// TODO: these map lookups are costly
	item, ok := c.deferredMessages[id]
	if !ok {
		c.deferredMutex.Unlock()
		return nil, errors.New("ID not deferred")
	}
	delete(c.deferredMessages, id)
	c.deferredMutex.Unlock()
	return item, nil
}

func (c *Channel) addToDeferredPQ(item *pqueue.Item[*Message, int64]) {
	c.deferredMutex.Lock()
	c.deferredPQ.Push(item)
	c.deferredMutex.Unlock()
}

func (c *Channel) processDeferredQueue(t int64) bool {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return false
	}

	dirty := false
	for {
		c.deferredMutex.Lock()
		item := c.deferredPQ.PeekAndShift(func(p int64) bool { return p > t })
		c.deferredMutex.Unlock()

		if item == nil {
			goto exit
		}
		dirty = true

		c.deferredMutex.Lock()
		delete(c.deferredMessages, item.Val.ID)
		c.deferredMutex.Unlock()

		c.put(item.Val)
	}

exit:
	return dirty
}

func (c *Channel) processInFlightQueue(t int64) bool {
	c.exitMutex.RLock()
	defer c.exitMutex.RUnlock()

	if c.Exiting() {
		return false
	}

	dirty := false
	for {
		c.inFlightMutex.Lock()
		item := c.inFlightPQ.PeekAndShift(func(p int64) bool { return p > t })
		c.inFlightMutex.Unlock()

		if item == nil {
			goto exit
		}
		dirty = true

		c.inFlightMutex.Lock()
		delete(c.inFlightMessages, item.Val.ID)
		c.inFlightMutex.Unlock()

		atomic.AddUint64(&c.timeoutCount, 1)
		c.RLock()
		client, ok := c.clients[item.Val.clientID]
		c.RUnlock()
		if ok {
			client.TimedOutMessage()
		}
		c.put(item.Val)
	}

exit:
	return dirty
}
