package queue

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"sync"
	"time"
)

var (
	ErrFull      = errors.New("fila cheia")
	ErrID        = errors.New("não foi possível gerar id para a mensagem")
	ErrNoTargets = errors.New("nenhum destino configurado")
)

type Message struct {
	ID         string
	Title      string
	Content    string
	Link       string
	Date       string
	ChannelIDs []string
	CreatedAt  time.Time
	ExpiresAt  time.Time
	Attempts   int
}

type entry struct {
	Message
	nextAttemptAt time.Time
	inFlight      bool
	sequence      uint64
}

type Queue struct {
	mu       sync.Mutex
	items    map[string]*entry
	capacity int
	ttl      time.Duration
	nextSeq  uint64
	wake     chan struct{}
}

func New(capacity int, ttl time.Duration) *Queue {
	return &Queue{
		items:    make(map[string]*entry),
		capacity: capacity,
		ttl:      ttl,
		wake:     make(chan struct{}, 1),
	}
}

func (q *Queue) Enqueue(title, content, link, date string, channelIDs []string) (Message, error) {
	id, err := newID()
	if err != nil {
		return Message{}, ErrID
	}
	if len(channelIDs) == 0 {
		return Message{}, ErrNoTargets
	}

	now := time.Now()
	message := Message{
		ID:         id,
		Title:      title,
		Content:    content,
		Link:       link,
		Date:       date,
		ChannelIDs: append([]string(nil), channelIDs...),
		CreatedAt:  now,
		ExpiresAt:  now.Add(q.ttl),
	}

	q.mu.Lock()
	q.expireLocked(now)
	if len(q.items) >= q.capacity {
		q.mu.Unlock()
		return Message{}, ErrFull
	}
	q.nextSeq++
	q.items[id] = &entry{
		Message:       message,
		nextAttemptAt: now,
		sequence:      q.nextSeq,
	}
	q.mu.Unlock()
	q.signal()

	return message, nil
}

func (q *Queue) Next(ctx context.Context) (Message, error) {
	for {
		q.mu.Lock()
		now := time.Now()
		q.expireLocked(now)

		var due *entry
		var nextWake time.Time
		for _, candidate := range q.items {
			if candidate.inFlight {
				continue
			}
			if !candidate.nextAttemptAt.After(now) {
				if due == nil || candidate.sequence < due.sequence {
					due = candidate
				}
				continue
			}
			if nextWake.IsZero() || candidate.nextAttemptAt.Before(nextWake) {
				nextWake = candidate.nextAttemptAt
			}
			if candidate.ExpiresAt.Before(nextWake) {
				nextWake = candidate.ExpiresAt
			}
		}
		if due != nil {
			due.inFlight = true
			due.Attempts++
			message := due.Message
			message.ChannelIDs = append([]string(nil), due.ChannelIDs...)
			q.mu.Unlock()
			return message, nil
		}
		q.mu.Unlock()

		if nextWake.IsZero() {
			select {
			case <-ctx.Done():
				return Message{}, ctx.Err()
			case <-q.wake:
			}
			continue
		}

		delay := time.Until(nextWake)
		if delay < 0 {
			delay = 0
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return Message{}, ctx.Err()
		case <-q.wake:
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
		case <-timer.C:
		}
	}
}

func (q *Queue) Complete(id string) {
	q.mu.Lock()
	delete(q.items, id)
	q.mu.Unlock()
	q.signal()
}

func (q *Queue) MarkDelivered(id, channelID string) {
	q.mu.Lock()
	if item, exists := q.items[id]; exists {
		for index, pendingChannelID := range item.ChannelIDs {
			if pendingChannelID == channelID {
				item.ChannelIDs = append(item.ChannelIDs[:index], item.ChannelIDs[index+1:]...)
				break
			}
		}
		if len(item.ChannelIDs) == 0 {
			delete(q.items, id)
		}
	}
	q.mu.Unlock()
	q.signal()
}

func (q *Queue) Retry(id string, at time.Time) {
	q.mu.Lock()
	if item, exists := q.items[id]; exists {
		if !item.ExpiresAt.After(time.Now()) {
			delete(q.items, id)
		} else {
			item.inFlight = false
			item.nextAttemptAt = at
		}
	}
	q.mu.Unlock()
	q.signal()
}

func (q *Queue) Expire() {
	q.mu.Lock()
	removed := q.expireLocked(time.Now())
	q.mu.Unlock()
	if removed {
		q.signal()
	}
}

func (q *Queue) expireLocked(now time.Time) bool {
	removed := false
	for id, item := range q.items {
		if !item.ExpiresAt.After(now) {
			delete(q.items, id)
			removed = true
		}
	}
	return removed
}

func (q *Queue) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

func newID() (string, error) {
	var data [16]byte
	if _, err := rand.Read(data[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(data[:]), nil
}
