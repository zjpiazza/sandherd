package lifecycle

import "sync"

type EventBus struct {
	mu          sync.Mutex
	capacity    int
	events      []Event
	subscribers map[chan Event]struct{}
}

func NewEventBus(capacity int) *EventBus {
	return &EventBus{capacity: capacity, subscribers: make(map[chan Event]struct{})}
}

func (b *EventBus) Publish(event Event) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if event.ID == "" {
		event.ID = NewID()
	}
	b.events = append(b.events, event)
	if len(b.events) > b.capacity {
		copy(b.events, b.events[len(b.events)-b.capacity:])
		b.events = b.events[:b.capacity]
	}
	for subscriber := range b.subscribers {
		select {
		case subscriber <- event:
		default:
			close(subscriber)
			delete(b.subscribers, subscriber)
		}
	}
}

func (b *EventBus) Subscribe(owner, afterID string) ([]Event, <-chan Event, bool, func()) {
	b.mu.Lock()
	defer b.mu.Unlock()
	start := 0
	if afterID != "" {
		found := false
		for index, event := range b.events {
			if event.ID == afterID {
				start = index + 1
				found = true
				break
			}
		}
		if !found {
			return nil, nil, false, func() {}
		}
	}
	replay := make([]Event, 0, len(b.events)-start)
	for _, event := range b.events[start:] {
		if event.Owner == owner {
			replay = append(replay, event)
		}
	}
	channel := make(chan Event, 64)
	b.subscribers[channel] = struct{}{}
	var once sync.Once
	cancel := func() {
		once.Do(func() {
			b.mu.Lock()
			defer b.mu.Unlock()
			if _, exists := b.subscribers[channel]; exists {
				delete(b.subscribers, channel)
				close(channel)
			}
		})
	}
	return replay, channel, true, cancel
}
