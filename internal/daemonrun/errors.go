package daemonrun

// ErrorKind identifies the stable command-level outcome of a daemon startup
// failure without making callers inspect rendered error text.
type ErrorKind uint8

const (
	ErrorInvalidInput ErrorKind = iota + 1
	ErrorOperational
	ErrorPartial
)

// Error carries a redacted user-facing message and retains the underlying
// failure for diagnostics and cancellation classification.
type Error struct {
	Kind    ErrorKind
	Message string
	Err     error
}

func (e *Error) Error() string {
	return e.Message
}

func (e *Error) Unwrap() error { return e.Err }

func failure(kind ErrorKind, message string, err error) error {
	return &Error{Kind: kind, Message: message, Err: err}
}
