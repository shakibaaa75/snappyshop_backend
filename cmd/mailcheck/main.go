// Command mailcheck sends one test email with the same SMTP_* settings the API
// uses, so you can confirm email works before sellers depend on it.
//
//	SMTP_HOST=smtp.gmail.com SMTP_PORT=587 SMTP_USERNAME=you@gmail.com \
//	SMTP_PASSWORD='app password' go run ./cmd/mailcheck you@example.com
//
// It prints the exact reason when something is wrong (bad login, blocked port,
// wrong From address) instead of failing silently.
package main

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"shopbd/backend/internal/mailer"
)

func main() {
	if len(os.Args) != 2 || !strings.Contains(os.Args[1], "@") {
		fmt.Fprintln(os.Stderr, "usage: mailcheck <recipient email>")
		os.Exit(2)
	}
	get := func(k, def string) string {
		if v := strings.TrimSpace(os.Getenv(k)); v != "" {
			return v
		}
		return def
	}
	cfg := mailer.Config{
		Host: get("SMTP_HOST", ""), Port: get("SMTP_PORT", "587"),
		Username: get("SMTP_USERNAME", ""), Password: os.Getenv("SMTP_PASSWORD"),
	}
	cfg.From = get("SMTP_FROM", cfg.Username)

	if cfg.Host == "" {
		fmt.Fprintln(os.Stderr, "SMTP_HOST is not set: the API would drop every email (seller email change and verification would be refused).")
		os.Exit(1)
	}
	if cfg.Username == "" || cfg.Password == "" {
		fmt.Fprintln(os.Stderr, "SMTP_USERNAME and SMTP_PASSWORD are required when SMTP_HOST is set.")
		os.Exit(1)
	}

	m := mailer.NewSender(cfg)
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	err := m.Send(ctx, os.Args[1], "ShopBD email check",
		"<p>This is a test email from ShopBD. Your SMTP settings work.</p>",
		"This is a test email from ShopBD. Your SMTP settings work.\r\n")
	if err != nil {
		fmt.Fprintln(os.Stderr, "FAILED:", err)
		os.Exit(1)
	}
	fmt.Printf("OK: sent to %s through %s:%s as %s\n", os.Args[1], cfg.Host, cfg.Port, cfg.From)
}
