package event

import (
	"context"
	"time"

	"github.com/xraph/cortex/id"
)

// FollowRequest bounds a metadata subscription. Zero durations select one-second
// polling, five-second reads and a five-minute lifetime. Hosts must honor context
// cancellation in both the authorized reader and the synchronous consumer.
type FollowRequest struct {
	RunID        id.AgentRunID
	After        string
	Limit        int
	PollInterval time.Duration
	ReadTimeout  time.Duration
	Lifetime     time.Duration
}

func (r FollowRequest) normalized() (FollowRequest, error) {
	if _, err := replayReadRequest(r.RunID, r.After, r.Limit); err != nil {
		return FollowRequest{}, err
	}
	if r.PollInterval == 0 {
		r.PollInterval = time.Second
	}
	if r.ReadTimeout == 0 {
		r.ReadTimeout = 5 * time.Second
	}
	if r.Lifetime == 0 {
		r.Lifetime = 5 * time.Minute
	}
	if r.PollInterval < 250*time.Millisecond || r.PollInterval > 30*time.Second || r.ReadTimeout <= 0 || r.ReadTimeout > 30*time.Second || r.Lifetime <= 0 || r.Lifetime > 5*time.Minute {
		return FollowRequest{}, ErrInvalid
	}
	return r, nil
}

// FollowReplay sends authorized pages synchronously, retaining at most one page.
// Each read rechecks authority, including idle polls. Consumer errors, gaps and
// cancellation stop the loop. It never infers completion from a lifecycle event.
// Pass an authorized engine, never a raw store; see ReadReplayPage.
func FollowReplay(ctx context.Context, authorized ReadStore, request FollowRequest, send func(context.Context, ReplayPage) error) error {
	request, err := request.normalized()
	if err != nil {
		return err
	}
	if nilPort(authorized) || send == nil {
		return ErrUnavailable
	}
	ctx, cancel := context.WithTimeout(ctx, request.Lifetime)
	defer cancel()
	for {
		if contextErr := replayDeadlineError(ctx); contextErr != nil {
			return contextErr
		}
		page, readErr := readFollowPage(ctx, authorized, request)
		if readErr != nil {
			return readErr
		}
		if contextErr := replayDeadlineError(ctx); contextErr != nil {
			return contextErr
		}
		if sendErr := send(ctx, page); sendErr != nil {
			return sendErr
		}
		if page.Gap {
			return nil
		}
		request.After = page.NextCursor
		// Drain a retained backlog immediately, then poll for new metadata.
		if page.Page.Next.Sequence < page.Page.Watermark.LastSequence {
			continue
		}
		timer := time.NewTimer(request.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func readFollowPage(ctx context.Context, authorized ReadStore, request FollowRequest) (ReplayPage, error) {
	ctx, cancel := context.WithTimeout(ctx, request.ReadTimeout)
	defer cancel()
	if err := replayDeadlineError(ctx); err != nil {
		return ReplayPage{}, err
	}
	page, err := ReadReplayPage(ctx, authorized, request.RunID, request.After, request.Limit)
	if deadlineErr := replayDeadlineError(ctx); deadlineErr != nil {
		return ReplayPage{}, deadlineErr
	}
	return page, err
}

// A context timer may become runnable alongside a poll timer. Check the clock
// as well as Err so delivery does not depend on which callback runs first.
func replayDeadlineError(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if deadline, ok := ctx.Deadline(); ok && !time.Now().Before(deadline) {
		return context.DeadlineExceeded
	}
	return nil
}
