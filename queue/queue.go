package queue

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
