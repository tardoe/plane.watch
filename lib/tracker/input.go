package tracker

import (
	"errors"
	"fmt"
	"plane.watch/lib/tracker/beast"
	"plane.watch/lib/tracker/mode_s"
	"plane.watch/lib/tracker/sbs1"
	"sync"
	"sync/atomic"
	"time"
)

type (
	// Option allows us to configure our new Tracker as we need it
	Option func(*Tracker)

	EventMaker interface {
		Stopper
		Listen() chan Event
	}
	EventListener interface {
		OnEvent(Event)
	}
	Stopper interface {
		Stop()
	}
	// Frame is our general object for a tracking update, AVR, SBS1, Modes Beast Binary
	Frame interface {
		Icao() uint32
		IcaoStr() string
		Decode() (bool, error)
		TimeStamp() time.Time
		Raw() []byte
	}

	// A Producer can listen for or generate Frames, it provides the output via a channel that the handler can then
	// processes further.
	// A Producer can send *LogEvent and  *FrameEvent events
	Producer interface {
		EventMaker
		fmt.Stringer
	}

	// Sink is something that takes the output from our producers and trackers
	Sink interface {
		EventListener
		Stopper
	}

	// Middleware has a chance to modify a frame before we send it to the plane Tracker
	Middleware interface {
		EventMaker
		fmt.Stringer
		Handle(Frame, *FrameSource) Frame
	}
)

func WithDecodeWorkerCount(numDecodeWorkers int) Option {
	return func(t *Tracker) {
		t.decodeWorkerCount = numDecodeWorkers
	}
}
func WithPruneTiming(pruneTick, pruneAfter time.Duration) Option {
	return func(t *Tracker) {
		t.pruneTick = pruneTick
		t.pruneAfter = pruneAfter
	}
}

// Finish begins the ending of the tracking by closing our decoding queue
func (t *Tracker) Finish() {
	for _, p := range t.producers {
		p.Stop()
	}
	for _, m := range t.middlewares {
		m.Stop()
	}
	close(t.decodingQueue)
	t.pruneExitChan <- true
	t.eventSync.Lock()
	t.eventsOpen = false
	t.eventSync.Unlock()

	close(t.events)
	for _, s := range t.sinks {
		s.Stop()
	}
}

func (t *Tracker) EventListener(eventSource EventMaker, waiter *sync.WaitGroup) {
	for e := range eventSource.Listen() {
		//fmt.Printf("Event For %s %s\n", eventSource, e)
		switch e.(type) {
		case *FrameEvent:
			t.decodingQueue <- e.(*FrameEvent)
			// send this event on!
			t.AddEvent(e)
		case *LogEvent:
			t.AddEvent(e)
		case *DedupedFrameEvent:
			t.AddEvent(e)
		}
	}
	waiter.Done()
	t.debugMessage("Done with Event Source %s", eventSource)
}

// AddProducer wires up a Producer to start feeding data into the tracker
func (t *Tracker) AddProducer(p Producer) {
	if nil == p {
		return
	}

	t.debugMessage("Adding producer: %s", p)
	t.producers = append(t.producers, p)
	t.producerWaiter.Add(1)

	go t.EventListener(p, &t.producerWaiter)
	t.debugMessage("Just added a producer")
}

// AddMiddleware wires up a Middleware which each message will go through before being added to the tracker
func (t *Tracker) AddMiddleware(m Middleware) {
	if nil == m {
		return
	}
	t.debugMessage("Adding middleware: %s", m)
	t.middlewares = append(t.middlewares, m)

	t.middlewareWaiter.Add(1)
	go t.EventListener(m, &t.middlewareWaiter)
	t.debugMessage("Just added a middleware")
}

// AddSink wires up a Sink in the tracker. Whenever an event happens it gets sent to each Sink
func (t *Tracker) AddSink(s Sink) {
	if nil == s {
		return
	}
	t.sinks = append(t.sinks, s)
}

// Stop attempts to stop all the things, mid flight. Use this if you have something else waiting for things to finish
// use this if you are listening to remote sources
func (t *Tracker) Stop() {
	t.Finish()
	t.producerWaiter.Wait()
	t.decodingQueueWaiter.Wait()
	t.eventsWaiter.Wait()
	t.middlewareWaiter.Wait()
}

// Wait waits for all producers to stop producing input and then returns
// use this method if you are processing a file
func (t *Tracker) Wait() {
	t.producerWaiter.Wait()
	time.Sleep(time.Millisecond * 50)
	t.Finish()
	t.decodingQueueWaiter.Wait()
	t.eventsWaiter.Wait()
}

func (t *Tracker) handleError(err error) {
	if nil != err {
		t.errorMessage("%s", err)
	}
}

func (t *Tracker) decodeQueue() {
	for f := range t.decodingQueue {
		if nil == f {
			continue
		}
		atomic.AddUint64(&t.numFrames, 1)
		frame := f.Frame()
		ok, err := frame.Decode()
		if nil != err {
			// the decode operation failed to produce valid output, and we tell someone about it
			t.handleError(err)
			continue
		}
		if !ok {
			// the decode operation did not produce a valid frame, but this is not an error
			// example: NoOp heartbeat
			continue
		}

		for _, m := range t.middlewares {
			frame = m.Handle(frame, f.source)
			if nil == frame {
				break
			}
		}
		if nil == frame || frame.Icao() == 0 {
			// invalid frame || unable to determine planes ICAO
			continue
		}
		plane := t.GetPlane(frame.Icao())

		switch frame.(type) {
		case *beast.Frame:
			plane.HandleModeSFrame(frame.(*beast.Frame).AvrFrame(), f.Source().RefLat, f.Source().RefLon)
			// todo: include signal strength
		case *mode_s.Frame:
			plane.HandleModeSFrame(frame.(*mode_s.Frame), f.Source().RefLat, f.Source().RefLon)
		case *sbs1.Frame:
			plane.HandleSbs1Frame(frame.(*sbs1.Frame))
		default:
			t.handleError(errors.New("unknown frame type, cannot track"))
		}
	}
	t.decodingQueueWaiter.Done()
}