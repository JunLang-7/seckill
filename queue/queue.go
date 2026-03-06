package queue

import (
	"context"
	"encoding/json"
	"errors"
	"log"

	amqp "github.com/rabbitmq/amqp091-go"
)

var ErrQueueFull = errors.New("order queue is full")

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
	// is cancelled (for RabbitMQ) or when the underlying store is closed (for
	// FakeQueue). Each call should be made by a separate goroutine/worker.
	Consume(ctx context.Context) (<-chan Delivery, error)
	Close()
}

// ─── RabbitMQ implementation ──────────────────────────────────────────────────

type OrderDeque struct {
	conn *amqp.Connection
	ch   *amqp.Channel
	q    amqp.Queue
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

	return &OrderDeque{conn: conn, ch: ch, q: q}
}

func (q *OrderDeque) Push(msg SeckillMessage) error {
	body, err := json.Marshal(msg)
	if err != nil {
		return err
	}

	return q.ch.PublishWithContext(
		context.Background(),
		"",
		q.q.Name,
		false,
		false,
		amqp.Publishing{
			DeliveryMode: amqp.Persistent,
			ContentType:  "application/json",
			Body:         body,
		})
}

// Consume opens a dedicated AMQP channel for the calling worker, wraps each
// incoming delivery in our Delivery type, and returns the channel. The channel
// is closed when ctx is cancelled. JSON-malformed messages are Nack'd and
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
