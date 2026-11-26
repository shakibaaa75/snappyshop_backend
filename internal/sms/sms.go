// Package sms is the seam for sending text messages (one-time codes).
//
// No SMS provider is wired in yet. Until one is, the server uses NotConfigured,
// which refuses to send and says so, instead of pretending the message went out.
// To add a provider, implement Sender (one method) and pick it in cmd/api/main.go.
package sms

import (
	"context"
	"errors"
	"log/slog"
)

// Sender delivers one text message. to is a Bangladesh mobile number in the
// local form 01XXXXXXXXX; a provider adapter converts it to whatever format
// its gateway wants (for example 8801XXXXXXXXX).
type Sender interface {
	Send(ctx context.Context, to, message string) error
}

// ErrNotConfigured means no SMS provider is set up on this server.
var ErrNotConfigured = errors.New("sms: no SMS provider is configured")

// NotConfigured is the default Sender. It never sends anything.
type NotConfigured struct{}

func (NotConfigured) Send(context.Context, string, string) error { return ErrNotConfigured }

// DevLog "sends" a message by writing it to the server log. Development only:
// it lets you finish the flow locally without a provider. main.go must never
// choose it in production.
type DevLog struct{ Log *slog.Logger }

func (d DevLog) Send(_ context.Context, to, message string) error {
	l := d.Log
	if l == nil {
		l = slog.Default()
	}
	l.Info("sms (development only, not actually sent)", "to", to, "message", message)
	return nil
}

// Configured reports whether s can really deliver messages. Only NotConfigured
// (or nil) says no; DevLog counts as configured for local work.
func Configured(s Sender) bool {
	if s == nil {
		return false
	}
	_, none := s.(NotConfigured)
	return !none
}
