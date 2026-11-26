package mailer

import (
	"context"
	"strings"
	"testing"
	"time"

	"shopbd/backend/internal/mailer/smtptest"
)

func newTestSender(t *testing.T, srv *smtptest.Server, user, pass string) Mailer {
	t.Helper()
	return NewSender(Config{Host: srv.Host, Port: srv.Port, Username: user, Password: pass, From: "ShopBD <no-reply@shopbd.test>"})
}

func TestSMTPDeliversAMessage(t *testing.T) {
	srv := smtptest.New(t, "mailer@shopbd.test", "apppassword")
	m := newTestSender(t, srv, "mailer@shopbd.test", "app password") // spaces in app passwords are ignored
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	err := m.Send(ctx, "new@example.com", "Your ShopBD verification code", "<b>123456</b>", "Your code is 123456\r\n")
	if err != nil {
		t.Fatal(err)
	}
	got := srv.Messages()
	if len(got) != 1 {
		t.Fatalf("messages = %d", len(got))
	}
	msg := got[0]
	if msg.From != "no-reply@shopbd.test" || len(msg.To) != 1 || msg.To[0] != "new@example.com" {
		t.Fatalf("envelope = %+v", msg)
	}
	if msg.Subject() != "Your ShopBD verification code" || !strings.Contains(msg.Text(), "123456") {
		t.Fatalf("subject=%q text=%q", msg.Subject(), msg.Text())
	}
}

func TestSMTPReportsBadCredentials(t *testing.T) {
	srv := smtptest.New(t, "mailer@shopbd.test", "right")
	m := newTestSender(t, srv, "mailer@shopbd.test", "wrong")
	err := m.Send(context.Background(), "a@example.com", "s", "<p>x</p>", "x")
	if err == nil || !strings.Contains(err.Error(), "smtp auth") {
		t.Fatalf("want an auth error that tells the admin what to check, got %v", err)
	}
	if len(srv.Messages()) != 0 {
		t.Fatal("nothing may be delivered after a failed login")
	}
}

func TestSMTPRejectsAnInvalidRecipient(t *testing.T) {
	srv := smtptest.New(t, "u@shopbd.test", "p")
	m := newTestSender(t, srv, "u@shopbd.test", "p")
	if err := m.Send(context.Background(), "not an address", "s", "h", "t"); err == nil {
		t.Fatal("want an error")
	}
}

func TestSMTPUnreachableServerIsAnError(t *testing.T) {
	m := NewSender(Config{Host: "127.0.0.1", Port: "1", Username: "u@x.test", Password: "p"})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := m.Send(ctx, "a@example.com", "s", "h", "t"); err == nil {
		t.Fatal("want an error, not a silent success")
	}
}
