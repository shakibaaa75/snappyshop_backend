package mailer

import "testing"

func TestConfigured(t *testing.T) {
	if Configured(nil) {
		t.Fatal("nil mailer must not count as configured")
	}
	// No SMTP host: NewSender returns the no-op mailer, which silently drops mail.
	if Configured(NewSender(Config{})) {
		t.Fatal("a Noop mailer (no SMTP_HOST) must not count as configured")
	}
	if !Configured(NewSender(Config{Host: "smtp.example.com", Port: "587", Username: "u@example.com", Password: "p"})) {
		t.Fatal("a real SMTP sender must count as configured")
	}
}
