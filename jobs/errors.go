package jobs

import (
	"errors"
	"fmt"
)

// Kind classifies why running a job failed. It is a small, fixed set, so it
// is safe as a log field or metric label.
type Kind string

const (
	// KindUnknownJob: no handler is registered for the envelope's name.
	KindUnknownJob Kind = "unknown_job"
	// KindDecode: the envelope or its payload could not be decoded into the
	// registered job type.
	KindDecode Kind = "decode"
	// KindUnsupportedVersion: the payload version is newer than this binary
	// handles, or older with no UpgradeFrom step to bring it forward.
	KindUnsupportedVersion Kind = "unsupported_version"
	// KindPanic: the handler panicked.
	KindPanic Kind = "panic"
	// KindHandler: the handler returned an error.
	KindHandler Kind = "handler"
)

// Sentinels for errors.Is. Each matches every Error of its Kind.
var (
	ErrUnknownJob         = errors.New("jobs: unknown job")
	ErrDecode             = errors.New("jobs: decode job")
	ErrUnsupportedVersion = errors.New("jobs: unsupported job version")
	ErrPanic              = errors.New("jobs: job handler panicked")
	ErrHandler            = errors.New("jobs: job handler failed")
)

var kindSentinels = map[Kind]error{
	KindUnknownJob:         ErrUnknownJob,
	KindDecode:             ErrDecode,
	KindUnsupportedVersion: ErrUnsupportedVersion,
	KindPanic:              ErrPanic,
	KindHandler:            ErrHandler,
}

// Error is a classified job failure. Err is the underlying cause (the
// handler's own error for KindHandler), reachable through errors.Is/As.
type Error struct {
	Kind    Kind
	Name    string
	Version int
	Err     error
}

func (e *Error) Error() string {
	subject := "job"
	if e.Name != "" {
		subject = fmt.Sprintf("job %q", e.Name)
	}
	switch e.Kind {
	case KindUnknownJob:
		if e.Err != nil {
			return fmt.Sprintf("jobs: no handler is registered for %s: %v", subject, e.Err)
		}
		return fmt.Sprintf("jobs: no handler is registered for %s", subject)
	case KindDecode:
		return fmt.Sprintf("jobs: decode %s: %v", subject, e.Err)
	case KindUnsupportedVersion:
		return fmt.Sprintf("jobs: %s version %d: %v", subject, e.Version, e.Err)
	case KindPanic:
		return fmt.Sprintf("jobs: %s panicked: %v", subject, e.Err)
	default:
		return fmt.Sprintf("jobs: %s failed: %v", subject, e.Err)
	}
}

// Unwrap returns the cause.
func (e *Error) Unwrap() error { return e.Err }

// Is matches the sentinel of the error's Kind.
func (e *Error) Is(target error) bool {
	return kindSentinels[e.Kind] == target
}

// Classify returns the Kind of a failure Registry.Run returned, or "" for
// nil. An error that is not a classified *Error is KindHandler.
//
// Classify, not errors.Is, is the authority on a failure's kind: a handler
// error that wraps another job's failure (a handler that runs a sub-job)
// is KindHandler, yet errors.Is still reaches the inner sentinel through the
// wrap chain.
func Classify(err error) Kind {
	if err == nil {
		return ""
	}
	var jobErr *Error
	if errors.As(err, &jobErr) {
		return jobErr.Kind
	}
	return KindHandler
}
