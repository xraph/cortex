package coordination

import "context"

type schedulerTicket struct {
	scope    string
	ready    chan struct{}
	admitted bool
	err      error
}

func (s *TaskScheduler) acquire(ctx context.Context, scope string) (*schedulerTicket, error) {
	s.mu.Lock()
	if s.stopped {
		s.mu.Unlock()
		return nil, ErrSchedulerStopped
	}
	if err := ctx.Err(); err != nil {
		s.mu.Unlock()
		return nil, err
	}
	if s.queued >= s.config.MaxQueued {
		s.mu.Unlock()
		return nil, ErrSchedulerFull
	}
	ticket := &schedulerTicket{scope: scope, ready: make(chan struct{})}
	if len(s.queues[scope]) == 0 {
		s.order = append(s.order, scope)
	}
	s.queues[scope] = append(s.queues[scope], ticket)
	s.queued++
	s.promote()
	s.mu.Unlock()
	select {
	case <-ticket.ready:
	case <-ctx.Done():
	}
	s.mu.Lock()
	if ticket.err != nil {
		s.mu.Unlock()
		return nil, ticket.err
	}
	if err := ctx.Err(); err != nil {
		if ticket.admitted {
			s.releaseLocked(ticket)
		} else {
			s.removeQueued(ticket)
			s.promote()
		}
		s.mu.Unlock()
		return nil, err
	}
	s.mu.Unlock()
	return ticket, nil
}

func (s *TaskScheduler) promote() {
	for !s.stopped && s.active < s.config.MaxConcurrent && len(s.order) > 0 {
		admitted := false
		for range len(s.order) {
			scope := s.order[0]
			s.order = s.order[1:]
			queue := s.queues[scope]
			if s.activeScope[scope] >= s.config.MaxPerScope {
				s.order = append(s.order, scope)
				continue
			}
			ticket := queue[0]
			queue = queue[1:]
			if len(queue) == 0 {
				delete(s.queues, scope)
			} else {
				s.queues[scope] = queue
				s.order = append(s.order, scope)
			}
			s.queued--
			s.active++
			s.activeScope[scope]++
			ticket.admitted = true
			close(ticket.ready)
			admitted = true
			break
		}
		if !admitted {
			return
		}
	}
}

func (s *TaskScheduler) removeQueued(ticket *schedulerTicket) {
	queue := s.queues[ticket.scope]
	for i, current := range queue {
		if current == ticket {
			queue = append(queue[:i], queue[i+1:]...)
			s.queued--
			break
		}
	}
	if len(queue) > 0 {
		s.queues[ticket.scope] = queue
		return
	}
	delete(s.queues, ticket.scope)
	for i, scope := range s.order {
		if scope == ticket.scope {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}
func (s *TaskScheduler) release(ticket *schedulerTicket) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.releaseLocked(ticket)
}
func (s *TaskScheduler) releaseLocked(ticket *schedulerTicket) {
	s.active--
	s.activeScope[ticket.scope]--
	if s.activeScope[ticket.scope] == 0 {
		delete(s.activeScope, ticket.scope)
	}
	s.promote()
	s.finishDrain()
}
func (s *TaskScheduler) finishDrain() {
	if s.stopped && s.active == 0 {
		s.drainOnce.Do(func() { close(s.drained) })
	}
}
