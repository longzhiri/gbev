package gbev

import (
	"errors"
)

var (
	ErrUnsupported             = errors.New("unsupported connection type")
	ErrConnAlreadyBound        = errors.New("connection already bound")
	ErrWriteBufferLimitReached = errors.New("write buffer limit reached")
	ErrBufevFreed              = errors.New("bufev already freed")
	ErrPollerClosed            = errors.New("poller closed")
	ErrEvtBaseClosed           = errors.New("event base closed")
)
