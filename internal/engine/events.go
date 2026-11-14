package engine

import (
	"context"
	"fmt"
	"time"

	"github.com/moby/moby/client"
	"github.com/skyri/ucd/internal/api"
)

// Run watches Docker events until ctx is canceled. It reconnects after failures.
// The caller owns the goroutine and must cancel and join it before Close.
func (b *Backend) Run(ctx context.Context) error {
	defer b.closeWindowsPorts()
	last := time.Now().UnixNano()
	backoff := time.Second
	for ctx.Err() == nil {
		since := fmt.Sprintf("%d.%09d", last/int64(time.Second), last%int64(time.Second))
		streamCtx, cancelStream := context.WithCancel(ctx)
		stream := b.docker.Events(streamCtx, client.EventsListOptions{Since: since})
		messages := stream.Messages
		var failure error
		var retry *time.Timer
		var retryC <-chan time.Time
		portError := false
		firstSnapshot := true
		retryDelay := time.Second
		stopRetry := func() {
			if retry != nil {
				retry.Stop()
			}
			retryC = nil
		}
		reconcile := func() {
			if b.id != "windows" {
				return
			}
			if err := b.reconcileWindowsPorts(ctx); err != nil {
				portError = true
				b.events.Publish(api.Event{Engine: b.id, Type: "engine.port_error", Message: "reconcile published ports: " + err.Error()})
				if retryC == nil {
					if retry == nil {
						retry = time.NewTimer(retryDelay)
					} else {
						retry.Reset(retryDelay)
					}
					retryC = retry.C
					if retryDelay < 30*time.Second {
						retryDelay *= 2
						if retryDelay > 30*time.Second {
							retryDelay = 30 * time.Second
						}
					}
				}
			} else {
				stopRetry()
				retryDelay = time.Second
				if portError || firstSnapshot {
					b.events.Publish(api.Event{Engine: b.id, Type: "engine.ports_reconciled", Message: "published ports reconciled"})
					portError = false
				}
			}
		}
		// The subscription is established before the snapshot, so lifecycle
		// events arriving during the snapshot are processed afterwards.
		reconcile()
		firstSnapshot = false
	streamLoop:
		for {
			select {
			case <-ctx.Done():
				stopRetry()
				cancelStream()
				return nil
			case <-retryC:
				retryC = nil
				reconcile()
			case <-b.portReconcile:
				reconcile()
			case event, ok := <-messages:
				if !ok {
					messages = nil
					continue
				}
				if event.TimeNano > last {
					last = event.TimeNano
				}
				backoff = time.Second
				if event.Type == "container" || event.Type == "network" {
					reconcile()
				}
				b.events.Publish(api.Event{Engine: b.id, Project: event.Actor.Attributes["com.docker.compose.project"], Type: "engine.event", Message: fmt.Sprintf("%s %s %s", event.Type, event.Action, event.Actor.ID)})
			case err, ok := <-stream.Err:
				if ok {
					failure = err
				}
				break streamLoop
			}
		}
		stopRetry()
		cancelStream()
		b.closeWindowsPorts()
		if ctx.Err() != nil {
			return nil
		}
		message := "Docker event stream disconnected"
		if failure != nil {
			message += ": " + failure.Error()
		}
		b.events.Publish(api.Event{Engine: b.id, Type: "engine.disconnected", Message: message})
		timer := time.NewTimer(backoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil
		case <-timer.C:
		}
		if backoff < 30*time.Second {
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
	return nil
}
