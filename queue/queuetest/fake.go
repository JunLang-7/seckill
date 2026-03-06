// Package queuetest provides an in-memory Queue implementation for use in
// tests. It is kept separate from the queue package so test helpers never
// appear in production builds.
package queuetest

import (
	"context"
	"seckill/queue"
)

// FakeQueue is a channel-backed queue.Queue. It does not require a running
// RabbitMQ broker. Ch is exported so tests can inspect buffered messages or
// push values directly.
//
// Behavioural differences from OrderDeque (acceptable in unit tests):
//   - No persistence — messages are lost if the process exits.
//   - No real ack/nack — both are no-ops; unacked messages are not requeued.
//   - Shutdown via Close() drains buffered messages before signalling EOF,
//     matching the "graceful drain" contract expected by worker tests.
type FakeQueue struct {
	Ch chan queue.SeckillMessage
}

func NewFakeQueue(cap int) *FakeQueue {
	return &FakeQueue{Ch: make(chan queue.SeckillMessage, cap)}
}

func (q *FakeQueue) Push(msg queue.SeckillMessage) error {
	select {
	case q.Ch <- msg:
		return nil
	default:
		return queue.ErrQueueFull
	}
}

// Consume returns a channel of Delivery values. The channel is closed when
// q.Ch is closed (buffered messages are drained first) or ctx is cancelled.
func (q *FakeQueue) Consume(ctx context.Context) (<-chan queue.Delivery, error) {
	out := make(chan queue.Delivery)
	go func() {
		defer close(out)
		for {
			select {
			case <-ctx.Done():
				return
			case msg, ok := <-q.Ch:
				if !ok {
					return
				}
				d := queue.NewDelivery(msg, func() {}, func(bool) {})
				select {
				case out <- d:
				case <-ctx.Done():
					return
				}
			}
		}
	}()
	return out, nil
}

// Close closes the underlying channel. Consume goroutines will drain any
// remaining buffered messages and then terminate.
func (q *FakeQueue) Close() {
	close(q.Ch)
}
