// Package mailer sends transactional emails over SMTP.
package mailer

import (
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
)

type Config struct {
	Host     string
	Port     string
	Username string
	Password string
	From     string
}

func SendPasswordReset(cfg Config, toEmail, code string) error {
	if cfg.Host == "" {
		return fmt.Errorf("mailer: SMTP_HOST not configured")
	}

	subject := "Reset your ShopBD password"
	body := fmt.Sprintf(
		"Hello,\r\n\r\n"+
			"Your password reset code is: %s\r\n\r\n"+
			"This code expires in 5 minutes. If you didn't request this, you can ignore this email.\r\n\r\n"+
			"— ShopBD",
		code,
	)

	msg := []byte(
		"From: " + cfg.From + "\r\n" +
			"To: " + toEmail + "\r\n" +
			"Subject: " + subject + "\r\n" +
			"MIME-Version: 1.0\r\n" +
			"Content-Type: text/plain; charset=\"UTF-8\"\r\n" +
			"\r\n" +
			body,
	)

	addr := net.JoinHostPort(cfg.Host, cfg.Port)

	if cfg.Port == "465" {
		return sendImplicitTLS(cfg, addr, toEmail, msg)
	}

	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	return smtp.SendMail(addr, auth, cfg.From, []string{toEmail}, msg)
}

func sendImplicitTLS(cfg Config, addr, toEmail string, msg []byte) error {
	host, _, _ := net.SplitHostPort(addr)
	tlsConn, err := tls.Dial("tcp", addr, &tls.Config{ServerName: host})
	if err != nil {
		return err
	}
	defer tlsConn.Close()

	client, err := smtp.NewClient(tlsConn, host)
	if err != nil {
		return err
	}
	defer client.Quit()

	auth := smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.Host)
	if err := client.Auth(auth); err != nil {
		return err
	}
	if err := client.Mail(cfg.From); err != nil {
		return err
	}
	if err := client.Rcpt(toEmail); err != nil {
		return err
	}
	w, err := client.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	return w.Close()
}

func NormalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}
