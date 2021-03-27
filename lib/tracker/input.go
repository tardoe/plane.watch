package tracker

import (
	"errors"
	"fmt"
	"plane.watch/lib/tracker/mode_s"
	"plane.watch/lib/tracker/sbs1"
	"time"
)

type (
	// Option allows us to configure our new Tracker as we need it
	Option func(*Tracker)

	// Frame is our general object for a tracking update, AVR, SBS1, Modes Beast Binary
	Frame interface {
		Icao() uint32
		IcaoStr() string
		Decode() (bool, error)
		TimeStamp() time.Time
	}
	// A Producer can listen for or generate Frames, it provides the output via a channel that the handler can then
	// processes further.
	// A Producer can send *LogEvent and  *FrameEvent events
	Producer interface {
		String() string
		Listen() chan Event
		Stop()
	}

	Sink interface {
		OnEvent(Event)
	}

	// a Middleware has a chance to modify a frame before we send it to the plane Tracker
	Middleware func(Frame) Frame
)

func (t *Tracker) setVerbosity(logLevel int) {
	fmt.Printf("Setting Verbosity To %d\n", logLevel)
	t.logLevel = logLevel
}

func WithVerboseOutput() Option {
	return func(ih *Tracker) {
		ih.setVerbosity(LogLevelDebug)
	}
}
func WithInfoOutput() Option {
	return func(ih *Tracker) {
		ih.setVerbosity(LogLevelInfo)
	}
}
func WithQuietOutput() Option {
	return func(ih *Tracker) {
		ih.setVerbosity(LogLevelQuiet)
	}
}
func WithDecodeWorkerCount(numDecodeWorkers int) Option {
	return func(ih *Tracker) {
		ih.decodeWorkerCount = numDecodeWorkers
	}
}

// Finish begins the ending of the tracking by closing our decoding queue
func (t *Tracker) Finish() {
	for _, p := range t.producers {
		p.Stop()
	}
	close(t.decodingQueue)
	t.pruneExitChan <- true
}

// AddProducer wires up a Producer to start feeding data into the tracker
func (t *Tracker) AddProducer(p Producer) {
	if nil == p {
		return
	}

	t.debugMessage("Adding producer: %s", p)
	t.producers = append(t.producers, p)
	t.producerWaiter.Add(1)

	go func() {
		for e := range p.Listen() {
			t.debugMessage("Producer (%s) Made Message: %s", p, e)
			switch e.(type) {
			case *FrameEvent:
				t.decodingQueue <- e.(*FrameEvent).frame
				// send this event on!
				t.AddEvent(e)
			case *LogEvent:
				if t.logLevel >= e.(*LogEvent).Level  {
					t.AddEvent(e)
				}
			}
		}
		t.producerWaiter.Done()
		t.debugMessage("Done with producer %s", p)
	}()
	t.debugMessage("Just added a producer")
}

// AddMiddleware wires up a Middleware which each message will go through before being added to the tracker
func (t *Tracker) AddMiddleware(m Middleware) {
	if nil == m {
		return
	}
	t.middlewaresLock.Lock()
	defer t.middlewaresLock.Unlock()
	t.middlewares = append(t.middlewares, m)
}

// AddSink wires up a Sink in the tracker. Whenever an event happens it gets sent to each Sink
func (t *Tracker) AddSink(s Sink) {
	if nil == s {
		return
	}
	t.sinksLock.Lock()
	defer t.sinksLock.Unlock()
	t.sinks = append(t.sinks, s)
}

// Wait waits for all producers to stop producing input and then returns
func (t *Tracker) Wait() {
	t.debugMessage("and we are up and running...")
	t.producerWaiter.Wait()
	t.debugMessage("All producers have exited, cleaning up and closing down")
	t.Finish()
	t.decodingQueueWaiter.Wait()
}

func (t *Tracker) handleError(err error) {
	if nil != err {
		t.errorMessage("%s", err)
	}
}

func (t *Tracker) decodeQueue() {
	for f := range t.decodingQueue {
		ok, err := f.Decode()
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

		t.middlewaresLock.RLock()
		for _, m := range t.middlewares {
			f = m(f)
		}
		t.middlewaresLock.RUnlock()

		switch f.(type) {
		case *mode_s.Frame:
			t.HandleModeSFrame(f.(*mode_s.Frame))
		case *sbs1.Frame:
			t.HandleSbs1Frame(f.(*sbs1.Frame))
		default:
			t.handleError(errors.New("unknown frame type, cannot track"))
		}
	}
	t.decodingQueueWaiter.Done()
}

func NewFrameEvent(f Frame) *FrameEvent {
	return &FrameEvent{frame: f}
}
