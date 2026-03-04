package queue_test

import (
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"seckill/queue"
)

func TestPush_Success(t *testing.T) {
	q := queue.NewOrderDeque(2)
	err := q.Push(queue.SeckillMessage{UserID: 1, ProductID: 10})
	require.NoError(t, err)
	assert.Len(t, q.Ch, 1)

	msg := <-q.Ch
	assert.Equal(t, 1, msg.UserID)
	assert.Equal(t, 10, msg.ProductID)
}

func TestPush_ReturnsErrQueueFullWhenBufferSaturated(t *testing.T) {
	q := queue.NewOrderDeque(1)
	require.NoError(t, q.Push(queue.SeckillMessage{UserID: 1, ProductID: 1}))

	err := q.Push(queue.SeckillMessage{UserID: 2, ProductID: 1})
	assert.ErrorIs(t, err, queue.ErrQueueFull)
}

// TestPush_ZeroCapacity verifies that a zero-capacity queue (unbuffered channel)
// always rejects pushes — simulates a fully saturated queue in fast-path tests.
func TestPush_ZeroCapacity(t *testing.T) {
	q := queue.NewOrderDeque(0)
	err := q.Push(queue.SeckillMessage{UserID: 1, ProductID: 1})
	assert.ErrorIs(t, err, queue.ErrQueueFull)
}

// TestPush_Concurrent sends 2× capacity messages from goroutines and verifies
// that exactly `cap` succeed — the rest get ErrQueueFull.
func TestPush_Concurrent(t *testing.T) {
	const cap = 100
	q := queue.NewOrderDeque(cap)

	var (
		wg      sync.WaitGroup
		mu      sync.Mutex
		success int
	)

	for i := 0; i < cap*2; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			if q.Push(queue.SeckillMessage{UserID: id, ProductID: 1}) == nil {
				mu.Lock()
				success++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()

	assert.Equal(t, cap, success, "exactly cap pushes should succeed")
	assert.Len(t, q.Ch, cap)
}
