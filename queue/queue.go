package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

var (
	ErrQueueFull     = errors.New("order queue is full")
	ErrPublishNacked = errors.New("broker nacked the message: durability not guaranteed")
	ErrPoolExhausted = errors.New("amqp channel pool exhausted")
)

const (
	// publishConfirmTimeout is the maximum time Push waits for a broker ACK.
	publishConfirmTimeout = 5 * time.Second
	poolSize              = 200
)

type SeckillMessage struct {
	UserID    int
	ProductID int
}

// Delivery wraps a parsed message with its acknowledgement callbacks so the
// worker never needs to import the amqp package directly.
type Delivery struct {
	Msg  SeckillMessage
	ack  func()
	nack func(requeue bool)
}

func (d Delivery) Ack()              { d.ack() }
func (d Delivery) Nack(requeue bool) { d.nack(requeue) }

// NewDelivery constructs a Delivery from the provided message and callbacks.
func NewDelivery(msg SeckillMessage, ack func(), nack func(bool)) Delivery {
	return Delivery{Msg: msg, ack: ack, nack: nack}
}

// Queue is the messaging abstraction used by the service and worker layers.
type Queue interface {
	Push(msg SeckillMessage) error
	// Consume returns a channel of deliveries. The channel is closed when ctx
	// is canceled (for RabbitMQ) or when the underlying store is closed (for
	// FakeQueue). Each call should be made by a separate goroutine/worker.
	Consume(ctx context.Context) (<-chan Delivery, error)
	Close()
}

// ─── RabbitMQ implementation ──────────────────────────────────────────────────

type OrderDeque struct {
	conn      *amqp.Connection
	queueName string
	chPool    chan *confirmChannel // pool of AMQP channels for publisher confirms
}

// confirmChannel bundles an AMQP channel with its confirms channel for pooling.
type confirmChannel struct {
	ch       *amqp.Channel
	confirms <-chan amqp.Confirmation
}

func NewOrderDeque(amqpURL string) *OrderDeque {
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ", err)
	}

	// init a channel to declare a queue
	initCh, err := conn.Channel()
	if err != nil {
		log.Fatal("Failed to open a channel", err)
	}

	q, err := initCh.QueueDeclare("sec-kill", true, false, false, false, nil)
	if err != nil {
		log.Fatal("Failed to declare a queue", err)
	}
	initCh.Close()

	// init Channel pool
	pool := make(chan *confirmChannel, poolSize)
	for i := 0; i < poolSize; i++ {
		ch, err := conn.Channel()
		if err != nil {
			log.Fatal("Failed to open a channel", err)
		}
		if err := ch.Confirm(false); err != nil {
			log.Fatal("Failed to enable publisher confirms", err)
		}
		confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))
		pool <- &confirmChannel{ch: ch, confirms: confirms}
	}

	return &OrderDeque{conn: conn, queueName: q.Name, chPool: pool}
}

func (q *OrderDeque) Push(msg SeckillMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// get a free channel from pool
	var cCh *confirmChannel
	select {
	case cCh = <-q.chPool:
	default:
		return ErrPoolExhausted
	}
	// return to channel back to pool
	defer func() { q.chPool <- cCh }()

	ctx, cancel := context.WithTimeout(context.Background(), publishConfirmTimeout)
	defer cancel()

	if err := cCh.ch.PublishWithContext(
		ctx,
		"",
		q.queueName,
		false,
		false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		},
	); err != nil {
		return fmt.Errorf("publish: %w", err)
	}

	// Block until the broker confirms persistence or the timeout fires.
	select {
	case confirm, ok := <-cCh.confirms:
		if !ok {
			return fmt.Errorf("confirm channel closed unexpectedly")
		}
		if !confirm.Ack {
			return ErrPublishNacked
		}
		return nil
	case <-ctx.Done():
		return fmt.Errorf("publisher confirm timeout after %s", publishConfirmTimeout)
	}
}

// Consume opens a dedicated AMQP channel for the calling worker, wraps each
// incoming delivery in our Delivery type, and returns the channel. The channel
// is closed when ctx is canceled. JSON-malformed messages are Nack'd and
// discarded so they never block the pipeline.
func (q *OrderDeque) Consume(ctx context.Context) (<-chan Delivery, error) {
	ch, err := q.conn.Channel()
	if err != nil {
		return nil, err
	}

	// One message at a time per worker — RabbitMQ round-robins across workers.
	if err := ch.Qos(1, 0, false); err != nil {
		ch.Close()
		return nil, err
	}

	amqpDeliveries, err := ch.Consume(
		q.queueName,
		"",
		false,
		false,
		false,
		false,
		nil,
	)
	if err != nil {
		ch.Close()
		return nil, err
	}

	out := make(chan Delivery)
	go func() {
		defer close(out)
		defer ch.Close()
		for {
			select {
			case <-ctx.Done():
				return
			case d, ok := <-amqpDeliveries:
				if !ok {
					return
				}
				var msg SeckillMessage
				if err := json.Unmarshal(d.Body, &msg); err != nil {
					log.Printf("[queue] malformed message, discarding: %v", err)
					d.Nack(false, false)
					continue
				}
				delivery := NewDelivery(
					msg,
					func() { d.Ack(false) },
					func(requeue bool) { d.Nack(false, requeue) },
				)
				select {
				case out <- delivery:
				case <-ctx.Done():
					d.Nack(false, true) // requeue — worker is shutting down
					return
				}
			}
		}
	}()
	return out, nil
}

// Close shuts down the channel pool and the underlying AMQP connection.
func (q *OrderDeque) Close() {
	close(q.chPool)
	for cCh := range q.chPool {
		if cCh.ch != nil {
			cCh.ch.Close()
		}
	}
	if q.conn != nil {
		q.conn.Close()
	}
}
