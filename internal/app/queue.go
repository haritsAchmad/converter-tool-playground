package app

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/redis/go-redis/v9"
)

var errQueueFull = errors.New("conversion queue is full")

type jobQueue interface {
	Enqueue(context.Context, string) error
	Dequeue(context.Context) (string, error)
	Ack(context.Context, string) error
	Depth() float64
	Close() error
}

type localJobQueue struct {
	jobs chan string
}

func newLocalJobQueue(size int) *localJobQueue {
	return &localJobQueue{jobs: make(chan string, size)}
}

func (q *localJobQueue) Enqueue(_ context.Context, id string) error {
	select {
	case q.jobs <- id:
		return nil
	default:
		return errQueueFull
	}
}

func (q *localJobQueue) Dequeue(ctx context.Context) (string, error) {
	select {
	case id := <-q.jobs:
		return id, nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func (q *localJobQueue) Depth() float64                    { return float64(len(q.jobs)) }
func (q *localJobQueue) Ack(context.Context, string) error { return nil }
func (q *localJobQueue) Close() error                      { return nil }

type redisJobQueue struct {
	client *redis.Client
	key    string
	active string
	limit  int64
}

func newJobQueue(cfg Config) (jobQueue, error) {
	if cfg.Mode == "standalone" {
		return newLocalJobQueue(cfg.QueueSize), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	queue, err := newRedisJobQueue(ctx, cfg.RedisURL, cfg.RedisQueue, cfg.QueueSize)
	if err != nil {
		return nil, err
	}
	if cfg.Mode == "worker" {
		if err := queue.requeueActive(ctx); err != nil {
			_ = queue.Close()
			return nil, fmt.Errorf("could not recover active redis jobs: %w", err)
		}
	}
	return queue, nil
}

func newRedisJobQueue(ctx context.Context, rawURL, key string, limit int) (*redisJobQueue, error) {
	opts, err := redis.ParseURL(rawURL)
	if err != nil {
		return nil, fmt.Errorf("invalid redis URL: %w", err)
	}
	client := redis.NewClient(opts)
	if err := client.Ping(ctx).Err(); err != nil {
		_ = client.Close()
		return nil, fmt.Errorf("redis unavailable: %w", err)
	}
	return &redisJobQueue{client: client, key: key, active: key + ":processing", limit: int64(limit)}, nil
}

var enqueueScript = redis.NewScript(`
if redis.call('LLEN', KEYS[1]) >= tonumber(ARGV[1]) then
  return 0
end
redis.call('RPUSH', KEYS[1], ARGV[2])
return 1
`)

func (q *redisJobQueue) Enqueue(ctx context.Context, id string) error {
	added, err := enqueueScript.Run(ctx, q.client, []string{q.key}, q.limit, id).Int()
	if err != nil {
		return err
	}
	if added == 0 {
		return errQueueFull
	}
	return nil
}

func (q *redisJobQueue) Dequeue(ctx context.Context) (string, error) {
	return q.client.BLMove(ctx, q.key, q.active, "LEFT", "RIGHT", 0).Result()
}

func (q *redisJobQueue) Ack(ctx context.Context, id string) error {
	return q.client.LRem(ctx, q.active, 1, id).Err()
}

var requeueActiveScript = redis.NewScript(`
local moved = 0
while true do
  local item = redis.call('RPOP', KEYS[2])
  if not item then return moved end
  redis.call('LPUSH', KEYS[1], item)
  moved = moved + 1
end
`)

func (q *redisJobQueue) requeueActive(ctx context.Context) error {
	return requeueActiveScript.Run(ctx, q.client, []string{q.key, q.active}).Err()
}

func (q *redisJobQueue) Depth() float64 {
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	n, err := q.client.LLen(ctx, q.key).Result()
	if err != nil {
		return 0
	}
	return float64(n)
}

func (q *redisJobQueue) Close() error { return q.client.Close() }
