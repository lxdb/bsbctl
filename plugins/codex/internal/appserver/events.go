package appserver

import (
	"context"
	"io"
	"slices"
	"time"
)

type pumpCommandKind uint8

const (
	pumpBegin pumpCommandKind = iota + 1
	pumpEmit
	pumpEnd
)

type pumpCommand struct {
	kind  pumpCommandKind
	event ManagerEvent
	done  chan struct{}
}

// eventPump is the sole event sender after connection initialization. Snapshot
// phases drain live input without delivering it until the baseline is complete.
// Both queues are bounded. Each RPC and event-delivery command has its own
// timeout so optional RPC failures do not cancel the remaining snapshot work.
type eventPump struct {
	session  *Session
	commands chan pumpCommand
	cancel   context.CancelFunc
	done     chan struct{}
}

func startEventPump(ctx context.Context, session *Session, events chan<- ManagerEvent) *eventPump {
	ctx, cancel := context.WithCancel(ctx)
	pump := &eventPump{session: session, commands: make(chan pumpCommand), cancel: cancel, done: make(chan struct{})}
	go pump.run(ctx, events)
	return pump
}

func (p *eventPump) stop() {
	p.cancel()
	<-p.done
}

func (p *eventPump) phase(ctx context.Context, snapshot func(context.Context, func(ManagerEvent) error) error) error {
	if err := p.command(ctx, pumpBegin, ManagerEvent{}); err != nil {
		return err
	}
	emit := func(event ManagerEvent) error { return p.command(ctx, pumpEmit, event) }
	if err := snapshot(ctx, emit); err != nil {
		return err
	}
	return p.command(ctx, pumpEnd, ManagerEvent{})
}

func (p *eventPump) command(ctx context.Context, kind pumpCommandKind, event ManagerEvent) error {
	ctx, cancel := context.WithTimeout(ctx, p.session.opts.RPCTimeout)
	defer cancel()
	command := pumpCommand{kind: kind, event: event, done: make(chan struct{})}
	select {
	case p.commands <- command:
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return io.ErrClosedPipe
	}
	select {
	case <-command.done:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-p.done:
		return io.ErrClosedPipe
	}
}

func (p *eventPump) run(ctx context.Context, events chan<- ManagerEvent) {
	defer close(p.done)
	queued := make([]ManagerEvent, 0, p.session.opts.EventBuffer)
	holding := true
	var pending, ending *pumpCommand
	stalled := time.NewTimer(p.session.opts.RPCTimeout)
	stalled.Stop()
	defer stalled.Stop()
	var stalledC <-chan time.Time
	for {
		if ending != nil && len(queued) == 0 && len(p.session.events) == 0 {
			close(ending.done)
			ending = nil
		}
		var output chan<- ManagerEvent
		var event ManagerEvent
		if pending != nil {
			output, event = events, pending.event
		} else if !holding && len(queued) != 0 {
			output, event = events, queued[0]
		}
		if output != nil && stalledC == nil {
			stalled.Reset(p.session.opts.RPCTimeout)
			stalledC = stalled.C
		} else if output == nil && stalledC != nil {
			stalled.Stop()
			stalledC = nil
		}
		var incoming <-chan Incoming
		if len(queued) < p.session.opts.EventBuffer {
			incoming = p.session.Events()
		}
		var commands <-chan pumpCommand
		if pending == nil && ending == nil {
			commands = p.commands
		}
		select {
		case <-ctx.Done():
			return
		case <-p.session.done:
			return
		case <-stalledC:
			p.session.finish(&ProtocolError{Kind: ProtocolEventBackpressure})
			return
		case value := <-incoming:
			queued = append(queued, ManagerEvent{Kind: ManagerIncoming, Incoming: value})
		case command := <-commands:
			switch command.kind {
			case pumpBegin:
				holding = true
				close(command.done)
			case pumpEmit:
				pending = &command
			case pumpEnd:
				holding = false
				ending = &command
			}
		case output <- event:
			stalled.Stop()
			stalledC = nil
			if pending != nil {
				close(pending.done)
				pending = nil
			} else {
				queued = slices.Delete(queued, 0, 1)
			}
		}
	}
}
