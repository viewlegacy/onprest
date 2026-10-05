package ws

import (
	"context"
	"errors"
	"sync"
)

type scheduled struct {
	ctx    context.Context
	run    func() error
	result chan error
}

// Scheduler keeps control and normal traffic bounded and interleaves transfer
// frames round robin. A job includes its entire frame write critical section.
type Scheduler struct {
	ctx             context.Context
	mu              sync.Mutex
	control, normal []scheduled
	transfers       map[string][]scheduled
	order           []string
	wake            chan struct{}
	failed          error
	lastNormal      bool
}

func NewScheduler(ctx context.Context) *Scheduler {
	return &Scheduler{ctx: ctx, transfers: map[string][]scheduled{}, wake: make(chan struct{}, 1)}
}
func (s *Scheduler) notify() {
	select {
	case s.wake <- struct{}{}:
	default:
	}
}
func (s *Scheduler) Post(ctx context.Context, priority bool, transfer string, run func() error) (<-chan error, error) {
	j := scheduled{ctx: ctx, run: run, result: make(chan error, 1)}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.failed != nil {
		return nil, s.failed
	}
	if s.ctx.Err() != nil {
		return nil, s.ctx.Err()
	}
	if priority {
		if len(s.control) >= 256 {
			return nil, errors.New("control queue is full")
		}
		s.control = append(s.control, j)
	} else if transfer == "" {
		if len(s.normal) >= 128 {
			return nil, errors.New("request queue is full")
		}
		s.normal = append(s.normal, j)
	} else {
		q := s.transfers[transfer]
		if len(q) >= 5 {
			return nil, errors.New("transfer queue is full")
		}
		if len(q) == 0 {
			s.order = append(s.order, transfer)
		}
		s.transfers[transfer] = append(q, j)
	}
	s.notify()
	return j.result, nil
}
func (s *Scheduler) Submit(ctx context.Context, priority bool, transfer string, run func() error) error {
	done, e := s.Post(ctx, priority, transfer, run)
	if e != nil {
		return e
	}
	select {
	case e := <-done:
		return e
	case <-ctx.Done():
		return ctx.Err()
	case <-s.ctx.Done():
		return s.ctx.Err()
	}
}

func (s *Scheduler) next() (scheduled, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var j scheduled
	if len(s.control) > 0 {
		j = s.control[0]
		s.control = s.control[1:]
		return j, true
	}
	if len(s.normal) > 0 && (len(s.order) == 0 || !s.lastNormal) {
		j = s.normal[0]
		s.normal = s.normal[1:]
		s.lastNormal = true
		return j, true
	}
	if len(s.order) > 0 {
		s.lastNormal = false
		id := s.order[0]
		s.order = s.order[1:]
		q := s.transfers[id]
		j = q[0]
		q = q[1:]
		if len(q) > 0 {
			s.order = append(s.order, id)
			s.transfers[id] = q
		} else {
			delete(s.transfers, id)
		}
		return j, true
	}
	return j, false
}
func (s *Scheduler) Run() error {
	for {
		j, ok := s.next()
		if ok {
			if s.ctx.Err() != nil {
				return s.ctx.Err()
			}
			if j.ctx.Err() != nil {
				j.result <- j.ctx.Err()
				continue
			}
			e := j.run()
			j.result <- e
			if e != nil && !errors.Is(e, context.Canceled) && !errors.Is(e, context.DeadlineExceeded) {
				s.mu.Lock()
				s.failed = e
				s.mu.Unlock()
				return e
			}
			continue
		}
		select {
		case <-s.wake:
		case <-s.ctx.Done():
			return s.ctx.Err()
		}
	}
}
