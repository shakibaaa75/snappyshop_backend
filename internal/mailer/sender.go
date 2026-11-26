package mailer

// This file ADDS a general-purpose sender to the existing mailer package.
// It reuses the existing Config type and does not touch SendPasswordReset,
// sendImplicitTLS or NormalizeEmail.

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log/slog"
	"mime"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"time"
)

// Mailer sends one email with an HTML body and a plain-text alternative.
type Mailer interface {
	Send(ctx context.Context, to, subject, htmlBody, textBody string) error
}

// Noop discards every email. Used when SMTP is not configured.
type Noop struct{}

func (Noop) Send(context.Context, string, string, string, string) error { return nil }

// Configured reports whether m can really deliver email. NewSender returns
// Noop (which silently drops every message and reports success) when SMTP is
// not set up; features that depend on the email actually arriving must check
// this first instead of trusting a nil error from Send.
func Configured(m Mailer) bool {
	if m == nil {
		return false
	}
	_, noop := m.(Noop)
	return !noop
}

type smtpSender struct {
	cfg Config
}

// NewSender returns a Mailer that uses the same SMTP Config as
// SendPasswordReset. If no SMTP host is configured it returns a no-op mailer,
// so the rest of the app keeps working and emails are simply skipped.
func NewSender(cfg Config) Mailer {
	if strings.TrimSpace(cfg.Host) == "" {
		slog.Warn("SMTP host is not configured: order emails are disabled")
		return Noop{}
	}
	return &smtpSender{cfg: cfg}
}

// Send delivers one message. Port 465 uses implicit TLS, any other port uses
// STARTTLS when the server offers it. The whole conversation has a hard
// deadline so a slow server cannot hang a goroutine.
func (s *smtpSender) Send(ctx context.Context, to, subject, htmlBody, textBody string) error {
	rcpt, err := mail.ParseAddress(strings.TrimSpace(to))
	if err != nil {
		return fmt.Errorf("invalid recipient %q: %w", to, err)
	}

	// cfg.From may be "Name <a@b.com>" or just "a@b.com"; fall back to the
	// SMTP username if it is empty or unparsable.
	from, err := mail.ParseAddress(strings.TrimSpace(s.cfg.From))
	if err != nil {
		from, err = mail.ParseAddress(strings.TrimSpace(s.cfg.Username))
		if err != nil {
			return fmt.Errorf("no valid From address: set the mailer From (or username) to an email address")
		}
	}

	msg, err := buildMessage(*from, rcpt.Address, subject, htmlBody, textBody)
	if err != nil {
		return fmt.Errorf("build message: %w", err)
	}

	host := strings.TrimSpace(s.cfg.Host)
	port := strings.TrimSpace(s.cfg.Port)
	if port == "" {
		port = "587"
	}
	addr := net.JoinHostPort(host, port)

	var conn net.Conn
	if port == "465" {
		d := tls.Dialer{
			NetDialer: &net.Dialer{Timeout: 15 * time.Second},
			Config:    &tls.Config{ServerName: host, MinVersion: tls.VersionTLS12},
		}
		conn, err = d.DialContext(ctx, "tcp", addr)
	} else {
		d := net.Dialer{Timeout: 15 * time.Second}
		conn, err = d.DialContext(ctx, "tcp", addr)
	}
	if err != nil {
		return fmt.Errorf("smtp dial %s: %w", addr, err)
	}
	_ = conn.SetDeadline(time.Now().Add(30 * time.Second))

	c, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("smtp client: %w", err)
	}
	defer c.Close()

	if port != "465" {
		if ok, _ := c.Extension("STARTTLS"); ok {
			if err := c.StartTLS(&tls.Config{ServerName: host, MinVersion: tls.VersionTLS12}); err != nil {
				return fmt.Errorf("smtp starttls: %w", err)
			}
		}
	}

	if s.cfg.Username != "" {
		// Google shows app passwords as "abcd efgh ijkl mnop"; spaces are not part of it.
		pass := strings.ReplaceAll(s.cfg.Password, " ", "")
		if err := c.Auth(smtp.PlainAuth("", s.cfg.Username, pass, host)); err != nil {
			return fmt.Errorf("smtp auth (check the username and app password): %w", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return fmt.Errorf("smtp MAIL FROM: %w", err)
	}
	if err := c.Rcpt(rcpt.Address); err != nil {
		return fmt.Errorf("smtp RCPT TO: %w", err)
	}
	w, err := c.Data()
	if err != nil {
		return fmt.Errorf("smtp DATA: %w", err)
	}
	if _, err := w.Write(msg); err != nil {
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("smtp close data: %w", err)
	}
	return c.Quit()
}

func buildMessage(from mail.Address, to, subject, htmlBody, textBody string) ([]byte, error) {
	boundary, err := randomBoundary()
	if err != nil {
		return nil, err
	}
	recipient := mail.Address{Address: to}

	var b bytes.Buffer
	header := func(k, v string) { b.WriteString(k + ": " + v + "\r\n") }
	header("From", from.String())
	header("To", recipient.String())
	header("Subject", mime.QEncoding.Encode("utf-8", stripCRLF(subject)))
	header("Date", time.Now().Format(time.RFC1123Z))
	header("MIME-Version", "1.0")
	header("Content-Type", `multipart/alternative; boundary="`+boundary+`"`)
	b.WriteString("\r\n")

	part := func(contentType, body string) error {
		b.WriteString("--" + boundary + "\r\n")
		b.WriteString("Content-Type: " + contentType + "; charset=utf-8\r\n")
		b.WriteString("Content-Transfer-Encoding: quoted-printable\r\n\r\n")
		qw := quotedprintable.NewWriter(&b)
		if _, err := qw.Write([]byte(body)); err != nil {
			return err
		}
		if err := qw.Close(); err != nil {
			return err
		}
		b.WriteString("\r\n")
		return nil
	}

	// Plain text first, HTML last: clients show the last alternative they support.
	if err := part("text/plain", textBody); err != nil {
		return nil, err
	}
	if err := part("text/html", htmlBody); err != nil {
		return nil, err
	}
	b.WriteString("--" + boundary + "--\r\n")
	return b.Bytes(), nil
}

func stripCRLF(s string) string {
	return strings.NewReplacer("\r", "", "\n", "").Replace(s)
}

func randomBoundary() (string, error) {
	buf := make([]byte, 16)
	if _, err := rand.Read(buf); err != nil {
		return "", err
	}
	return "shopbd-" + hex.EncodeToString(buf), nil
}
