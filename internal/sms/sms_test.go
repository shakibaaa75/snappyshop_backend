package sms

import (
	"context"
	"errors"
	"testing"
)

func TestNotConfiguredRefusesToSend(t *testing.T) {
	var s Sender = NotConfigured{}
	if err := s.Send(context.Background(), "01712345678", "code 123456"); !errors.Is(err, ErrNotConfigured) {
		t.Fatalf("err = %v, want ErrNotConfigured", err)
	}
	if Configured(s) || Configured(nil) {
		t.Fatal("NotConfigured / nil must report not configured")
	}
	if !Configured(DevLog{}) {
		t.Fatal("DevLog counts as configured for local work")
	}
	if err := (DevLog{}).Send(context.Background(), "01712345678", "hi"); err != nil {
		t.Fatalf("DevLog.Send: %v", err)
	}
}
