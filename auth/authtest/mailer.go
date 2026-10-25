// Package authtest holds test helpers for applications that use the
// framework's auth: a Mailer that records the account lifecycle's messages
// instead of sending them, so a test can read the token a flow mailed.
package authtest

import (
	"context"
	"sync"

	"github.com/gombit-dev/gombit/auth"
)

// Mailer is an auth.Mailer that records every message, for tests. Set Err
// to make Send fail. It is safe for concurrent use.
//
//	mail := &authtest.Mailer{}
//	app, _ := framework.New(framework.WithAuthMailer(mail), ...)
//	// register ada@example.com, then:
//	token := mail.Last("ada@example.com").Token
type Mailer struct {
	mu   sync.Mutex
	sent []auth.Message
	// Err, when set, is returned by Send, and the message is not recorded.
	Err error
}

// Send records msg, or returns Err.
func (m *Mailer) Send(_ context.Context, msg auth.Message) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.Err != nil {
		return m.Err
	}
	m.sent = append(m.sent, msg)
	return nil
}

// Messages returns the recorded messages, oldest first.
func (m *Mailer) Messages() []auth.Message {
	m.mu.Lock()
	defer m.mu.Unlock()
	return append([]auth.Message(nil), m.sent...)
}

// Last returns the latest message sent to address, and whether there is one.
func (m *Mailer) Last(address string) (auth.Message, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := len(m.sent) - 1; i >= 0; i-- {
		if m.sent[i].To == address {
			return m.sent[i], true
		}
	}
	return auth.Message{}, false
}

// Reset forgets the recorded messages.
func (m *Mailer) Reset() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sent = nil
}
