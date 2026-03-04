package queue

import "errors"

var ErrQueueFull = errors.New("order queue is full")

type SeckillMessage struct {
	UserID    int
	ProductID int
}

type OrderDeque struct {
	Ch chan SeckillMessage
}

func NewOrderDeque(bufSize int) *OrderDeque {
	return &OrderDeque{Ch: make(chan SeckillMessage, bufSize)}
}

func (q *OrderDeque) Push(msg SeckillMessage) error {
	select {
	case q.Ch <- msg:
		return nil
	default:
		return ErrQueueFull
	}
}
