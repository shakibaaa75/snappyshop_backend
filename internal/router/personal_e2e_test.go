package router

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"shopbd/backend/internal/config"
	"shopbd/backend/internal/mailer"
	"shopbd/backend/internal/mailer/smtptest"
	"shopbd/backend/internal/otp"
	"shopbd/backend/internal/password"
	"shopbd/backend/internal/redisx"
	"shopbd/backend/internal/repository/memrepo"
	"shopbd/backend/internal/service"
	"shopbd/backend/internal/sms"
)

// End to end through the real HTTP router and the REAL SMTP sender, talking to
// an in-process SMTP server. Codes are read out of the delivered emails, like a
// person would. Runs on the in-memory code store always, and again on real
// Redis when REDIS_TEST_URL is set.
func TestPersonalInfoEndToEnd(t *testing.T) {
	stores := map[string]func(t *testing.T) otp.Store{
		"memory": func(*testing.T) otp.Store { return otp.NewMemory() },
	}
	if url := os.Getenv("REDIS_TEST_URL"); url != "" {
		stores["redis"] = func(t *testing.T) otp.Store {
			c, err := redisx.Open(redisx.Config{URL: url})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = c.Close() })
			return otp.NewRedis(c, fmt.Sprintf("test:e2e:%d:", time.Now().UnixNano()), time.Second)
		}
	}
	for name, mk := range stores {
		t.Run(name, func(t *testing.T) { personalEndToEnd(t, mk(t)) })
	}
}

func personalEndToEnd(t *testing.T, store otp.Store) {
	smtp := smtptest.New(t, "mailer@shopbd.test", "secret")
	sender := mailer.NewSender(mailer.Config{Host: smtp.Host, Port: smtp.Port,
		Username: "mailer@shopbd.test", Password: "secret", From: "ShopBD <no-reply@shopbd.test>"})
	if !mailer.Configured(sender) {
		t.Fatal("SMTP sender should count as configured")
	}

	smsSink := &routeSMS{}
	users, tokens := memrepo.NewUsers(), memrepo.NewTokens()
	cfg := &config.Config{Env: "test", JWTSecret: []byte(strings.Repeat("k", 40)),
		AccessTTL: 15 * time.Minute, RefreshTTL: 24 * time.Hour, CORSOrigins: []string{frontendOrigin}}
	hasher := password.New(password.Params{Memory: 8, Time: 1, Threads: 1, KeyLen: 32, SaltLen: 16})
	auth, err := service.NewAuthService(users, tokens, hasher,
		service.AuthConfig{JWTSecret: cfg.JWTSecret, AccessTTL: cfg.AccessTTL, RefreshTTL: cfg.RefreshTTL})
	if err != nil {
		t.Fatal(err)
	}
	big := LimitSpec{Every: time.Millisecond, Burst: 1000}
	d := Deps{Config: cfg, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Auth: auth,
		Limits:       &Limits{Register: big, Login: big, LoginEmail: big, Refresh: big, PersonalSend: big, PersonalConfirm: big},
		PersonalInfo: service.NewPersonalInfoService(users, hasher, store, sender, smsSink, cfg.JWTSecret)}
	srv := httptest.NewServer(New(d))
	t.Cleanup(srv.Close)
	jar, _ := cookiejar.New(nil)
	e := &env{srv: srv, client: &http.Client{Jar: jar}, jar: jar, users: users}

	// codeFor waits for the email to arrive over SMTP and returns the code in it.
	codeFor := func(to string, n int) string {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var hits []smtptest.Message
			for _, m := range smtp.Messages() {
				if len(m.To) == 1 && m.To[0] == to {
					hits = append(hits, m)
				}
			}
			if len(hits) >= n {
				return routeCodeRE.FindString(hits[n-1].Text())
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Fatalf("no email #%d to %s; got %d messages", n, to, len(smtp.Messages()))
		return ""
	}

	// 1. A seller signs up, signs in, and sees their private details.
	if r := e.post(t, "/api/v1/auth/register", registerBody("john@example.com")); r.StatusCode != http.StatusCreated {
		t.Fatalf("register = %d %s", r.StatusCode, r.body)
	}
	b := decodePersonal(t, e.get(t, "/api/v1/seller/personal"))
	if b.Personal.Email != "john@example.com" || !b.Personal.Availability.Email {
		t.Fatalf("personal = %+v", b.Personal)
	}

	// 2. Change the email: the code is emailed to the NEW address only.
	r := e.post(t, "/api/v1/seller/personal/email/change", map[string]string{"newEmail": "john.new@example.com", "currentPassword": "password1"})
	if r.StatusCode != 200 {
		t.Fatalf("request = %d %s", r.StatusCode, r.body)
	}
	code := codeFor("john.new@example.com", 1)
	for _, m := range smtp.Messages() {
		if m.To[0] == "john@example.com" {
			t.Fatal("the old address must not get the code")
		}
	}
	if r := e.post(t, "/api/v1/seller/personal/email/change/confirm", map[string]string{"newEmail": "john.new@example.com", "code": code}); r.StatusCode != 200 {
		t.Fatalf("confirm = %d %s", r.StatusCode, r.body)
	}

	// 3. The previous address is notified over SMTP, with the new one masked.
	deadline := time.Now().Add(5 * time.Second)
	var notice smtptest.Message
	for time.Now().Before(deadline) && notice.Raw == "" {
		for _, m := range smtp.Messages() {
			if m.To[0] == "john@example.com" {
				notice = m
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	if notice.Raw == "" {
		t.Fatal("previous address was not notified")
	}
	if !strings.Contains(notice.Subject(), "changed") || !strings.Contains(notice.Text(), "j***@example.com") || strings.Contains(notice.Text(), "john.new@example.com") {
		t.Fatalf("notice = %q / %q", notice.Subject(), notice.Text())
	}

	// 4. Sign-in follows the new address; the old one is gone.
	e.post(t, "/api/v1/auth/logout", nil)
	if r := e.post(t, "/api/v1/auth/login", map[string]string{"email": "john@example.com", "password": "password1"}); r.StatusCode != 401 {
		t.Fatalf("old email login = %d", r.StatusCode)
	}
	if r := e.post(t, "/api/v1/auth/login", map[string]string{"email": "john.new@example.com", "password": "password1"}); r.StatusCode != 200 {
		t.Fatalf("new email login = %d %s", r.StatusCode, r.body)
	}

	// 5. The profile picture shows up in /auth/me (the header reads it from there).
	u, _ := users.FindByEmail(context.Background(), "john.new@example.com")
	if err := users.SetAvatar(context.Background(), u.ID, "https://cdn.test/seller_avatar/x/a.webp"); err != nil {
		t.Fatal(err)
	}
	var me struct {
		User struct {
			Email     string `json:"email"`
			AvatarURL string `json:"avatarUrl"`
		} `json:"user"`
	}
	if err := json.Unmarshal(e.get(t, "/api/v1/auth/me").body, &me); err != nil {
		t.Fatal(err)
	}
	if me.User.Email != "john.new@example.com" || me.User.AvatarURL != "https://cdn.test/seller_avatar/x/a.webp" {
		t.Fatalf("/auth/me = %+v", me.User)
	}

	// 6. Phone change (a fake SMS provider stands in; real phone verification
	// providers come later): password + number -> code by SMS -> saved & verified.
	r = e.post(t, "/api/v1/seller/personal/phone/change", map[string]string{"phone": "+8801712345678", "currentPassword": "password1"})
	if r.StatusCode != 200 {
		t.Fatalf("phone request = %d %s", r.StatusCode, r.body)
	}
	pcode := routeCodeRE.FindString(smsSink.message())
	r = e.post(t, "/api/v1/seller/personal/phone/change/confirm", map[string]string{"phone": "01712345678", "code": pcode})
	if r.StatusCode != 200 {
		t.Fatalf("phone confirm = %d %s", r.StatusCode, r.body)
	}
	if pb := decodePersonal(t, r); pb.Personal.Phone != "01712345678" || !pb.Personal.PhoneVerified {
		t.Fatalf("phone = %+v", pb.Personal)
	}
}

var _ sms.Sender = (*routeSMS)(nil)
