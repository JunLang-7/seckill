package queue

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"sync"
	"time"

	amqp "github.com/rabbitmq/amqp091-go"
)

var (
	ErrQueueFull     = errors.New("order queue is full")
	ErrPublishNacked = errors.New("broker nacked the message: durability not guaranteed")
)

// publishConfirmTimeout is the maximum time Push waits for a broker ACK.
const publishConfirmTimeout = 5 * time.Second

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
	conn     *amqp.Connection
	ch       *amqp.Channel
	q        amqp.Queue
	mu       sync.Mutex
	confirms chan amqp.Confirmation // broker ACK/NACK stream (Publisher Confirms)
}

func NewOrderDeque(amqpURL string) *OrderDeque {
	conn, err := amqp.Dial(amqpURL)
	if err != nil {
		log.Fatal("Failed to connect to RabbitMQ", err)
	}

	ch, err := conn.Channel()
	if err != nil {
		log.Fatal("Failed to open a channel", err)
	}

	q, err := ch.QueueDeclare("sec-kill", true, false, false, false, nil)
	if err != nil {
		log.Fatal("Failed to declare a queue", err)
	}

	// enable publisher confirms mode
	if err := ch.Confirm(false); err != nil {
		log.Fatal("Failed to enable publisher confirms", err)
		return nil
	}

	// Buffer of 1 is sufficient because Push holds mu for the full
	// publish-then-wait cycle, so at most one confirm is ever in flight.
	confirms := ch.NotifyPublish(make(chan amqp.Confirmation, 1))

	return &OrderDeque{conn: conn, ch: ch, q: q, confirms: confirms}
}

func (q *OrderDeque) Push(msg SeckillMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	// The mutex serialises concurrent Push calls so that each publish's
	// delivery-tag maps unambiguously to the very next item on q.confirms.
	q.mu.Lock()
	defer q.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), publishConfirmTimeout)
	defer cancel()

	if err := q.ch.PublishWithContext(
		ctx,
		"",
		q.q.Name,
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
	case confirm, ok := <-q.confirms:
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
		q.q.Name,
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

// Close shuts down the publishing channel and the underlying AMQP connection.
func (q *OrderDeque) Close() {
	if q.ch != nil {
		q.ch.Close()
	}
	if q.conn != nil {
		q.conn.Close()
	}
}
