package ringbuffer

import (
	"github.com/cloudfoundry/gosteno"
	"github.com/cloudfoundry/loggregatorlib/logmessage"
	"loggregator/buffer"
	"sync"
)

type ringBuffer struct {
	inputChannel  <-chan *logmessage.Message
	outputChannel chan *logmessage.Message
	logger        *gosteno.Logger
	lock          *sync.RWMutex
}

func NewRingBuffer(inputChannel <-chan *logmessage.Message, bufferSize uint, logger *gosteno.Logger) buffer.MessageBuffer {
	outputChannel := make(chan *logmessage.Message, bufferSize)
	return &ringBuffer{inputChannel, outputChannel, logger, &sync.RWMutex{}}
}

func (r *ringBuffer) SetOutputChannel(out chan *logmessage.Message) {
	r.lock.Lock()
	defer r.lock.Unlock()

	r.outputChannel = out
}

func (r *ringBuffer) GetOutputChannel() <-chan *logmessage.Message {
	r.lock.Lock()
	defer r.lock.Unlock()

	return r.outputChannel
}

func (r *ringBuffer) CloseOutputChannel() {
	close(r.outputChannel)
}

func (r *ringBuffer) Run() {
	for v := range r.inputChannel {
		r.lock.Lock()
		select {
		case r.outputChannel <- v:
		default:
			<-r.outputChannel
			r.outputChannel <- v
			if r.logger != nil {
				r.logger.Warnf("RBC: Reader was too slow. Dropped message.")
			}
		}
		r.lock.Unlock()
	}
	close(r.outputChannel)
}
