package timequeue

import (
	"container/heap"
	"sync"
	"time"
)

const (
	DefaultSize = 1
)

type TimeQueue struct {
	messageLock *sync.RWMutex
	messageHeap messageHeap

	stateLock  *sync.RWMutex
	running    bool
	wakeSignal *wakeSignal

	messageChan chan *Message
	stopChan    chan struct{}
	wakeChan    chan time.Time
}

func New() *TimeQueue {
	return NewSize(DefaultSize)
}

func NewSize(size int) *TimeQueue {
	q := &TimeQueue{
		messageLock: &sync.RWMutex{},
		messageHeap: messageHeap([]*Message{}),
		stateLock:   &sync.RWMutex{},
		running:     false,
		wakeSignal:  nil,
		messageChan: make(chan *Message, size),
		stopChan:    make(chan struct{}),
		wakeChan:    make(chan time.Time),
	}
	heap.Init(&q.messageHeap)
	return q
}

func (q *TimeQueue) Push(time time.Time, data interface{}) *Message {
	message := &Message{
		Time: time,
		Data: data,
	}
	q.PushMessage(message)
	return message
}

func (q *TimeQueue) PushMessage(message *Message) {
	if message == nil {
		return
	}
	heap.Push(&q.messageHeap, message)
	defer q.afterHeapUpdate()
}

func (q *TimeQueue) Peek() (time.Time, interface{}) {
	message := q.PeekMessage()
	if message == nil {
		return time.Time{}, nil
	}
	return message.Time, message.Data
}

func (q *TimeQueue) PeekMessage() *Message {
	q.messageLock.RLock()
	defer q.messageLock.RUnlock()
	if len(q.messageHeap) > 0 {
		return q.messageHeap[0]
	}
	return nil
}

func (q *TimeQueue) Pop(release bool) *Message {
	defer q.afterHeapUpdate()
	q.messageLock.Lock()
	defer q.messageLock.Unlock()
	if len(q.messageHeap) == 0 {
		return nil
	}
	message := heap.Pop(&q.messageHeap).(*Message)
	if release {
		q.releaseMessage(message)
	}
	return message
}

func (q *TimeQueue) PopAll(release bool) []*Message {
	defer q.afterHeapUpdate()
	q.messageLock.Lock()
	defer q.messageLock.Unlock()
	result := make([]*Message, 0, q.messageHeap.Len())
	for q.messageHeap.Len() > 0 {
		message := heap.Pop(&q.messageHeap).(*Message)
		result = append(result, message)
	}
	if release {
		q.releaseCopyToChan(result)
	}
	return result
}

func (q *TimeQueue) PopAllUntil(until time.Time, release bool) []*Message {
	defer q.afterHeapUpdate()
	q.messageLock.Lock()
	defer q.messageLock.Unlock()
	result := make([]*Message, 0, q.messageHeap.Len())
	for q.messageHeap.Len() > 0 {
		message := q.messageHeap[0]
		if message.Time.Sub(until) >= 0 {
			break
		}
		result = append(result, heap.Pop(&q.messageHeap).(*Message))
	}
	if release {
		q.releaseCopyToChan(result)
	}
	return result
}

func (q *TimeQueue) afterHeapUpdate() {
	if q.IsRunning() {
		q.updateAndSpawnWakeSignal()
	}
}

func (q *TimeQueue) Messages() <-chan *Message {
	return q.messageChan
}

func (q *TimeQueue) Size() int {
	q.messageLock.RLock()
	defer q.messageLock.RUnlock()
	return q.messageHeap.Len()
}

func (q *TimeQueue) Start() {
	if q.IsRunning() {
		return
	}
	q.setRunning(true)
	go q.run()
	q.updateAndSpawnWakeSignal()
}

func (q *TimeQueue) IsRunning() bool {
	q.stateLock.RLock()
	defer q.stateLock.RUnlock()
	return q.running
}

func (q *TimeQueue) run() {
runLoop:
	for {
		select {
		case wakeTime := <-q.wakeChan:
			q.onWake(wakeTime)
		case <-q.stopChan:
			break runLoop
		}
	}
}

func (q *TimeQueue) onWake(wakeTime time.Time) {
	q.releaseUntil(wakeTime)
	q.setWakeSignal(nil)
	q.updateAndSpawnWakeSignal()
}

func (q *TimeQueue) releaseUntil(until time.Time) {
	q.PopAllUntil(until, true)
}

func (q *TimeQueue) releaseMessage(message *Message) {
	go func() {
		q.messageChan <- message
	}()
}

func (q *TimeQueue) releaseCopyToChan(messages []*Message) {
	copyChan := make(chan *Message, len(messages))
	for _, message := range messages {
		copyChan <- message
	}
	q.releasecChan(copyChan)
	close(copyChan)
}

func (q *TimeQueue) releasecChan(messages <-chan *Message) {
	go func() {
		for message := range messages {
			q.messageChan <- message
		}
	}()
}

func (q *TimeQueue) updateAndSpawnWakeSignal() {
	q.killWakeSignal()
	message := q.PeekMessage()
	if message == nil {
		return
	}
	q.setWakeSignal(newWakeSignal(q.wakeChan, message.Time))
	q.spawnWakeSignal()
}

func (q *TimeQueue) setWakeSignal(wakeSignal *wakeSignal) {
	q.stateLock.Lock()
	defer q.stateLock.Unlock()
	q.wakeSignal = wakeSignal
}

func (q *TimeQueue) spawnWakeSignal() {
	q.stateLock.RLock()
	defer q.stateLock.RUnlock()
	if q.wakeSignal != nil {
		q.wakeSignal.spawn()
	}
}

func (q *TimeQueue) killWakeSignal() {
	q.stateLock.RLock()
	defer q.stateLock.RUnlock()
	if q.wakeSignal != nil {
		q.wakeSignal.kill()
	}
}

func (q *TimeQueue) Stop() {
	if !q.IsRunning() {
		return
	}
	q.killWakeSignal()
	q.setRunning(false)
	go func() {
		q.stopChan <- struct{}{}
	}()
}

func (q *TimeQueue) setRunning(running bool) {
	q.stateLock.Lock()
	defer q.stateLock.Unlock()
	q.running = running
}

type wakeSignal struct {
	dst  chan time.Time
	src  <-chan time.Time
	stop chan struct{}
}

func newWakeSignal(dst chan time.Time, wakeTime time.Time) *wakeSignal {
	return &wakeSignal{
		dst:  dst,
		src:  time.After(wakeTime.Sub(time.Now())),
		stop: make(chan struct{}),
	}
}

func (w *wakeSignal) spawn() {
	go func() {
		select {
		case wakeTime := <-w.src:
			w.dst <- wakeTime
		case <-w.stop:
		}
		w.src = nil
	}()
}

func (w *wakeSignal) kill() {
	close(w.stop)
}
