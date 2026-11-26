// Package smtptest is a tiny in-process SMTP server for tests. It speaks just
// enough SMTP (EHLO, AUTH PLAIN, MAIL, RCPT, DATA, QUIT) for the real
// mailer.NewSender to deliver to it, and keeps every message it receives.
package smtptest

import (
	"bufio"
	"encoding/base64"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
)

// Message is one delivered email.
type Message struct {
	From string
	To   []string
	Raw  string
}

// Subject is the decoded Subject header.
func (m Message) Subject() string {
	msg, err := mail.ReadMessage(strings.NewReader(m.Raw))
	if err != nil {
		return ""
	}
	s, _ := new(mime.WordDecoder).DecodeHeader(msg.Header.Get("Subject"))
	return s
}

// Text is the decoded text/plain part.
func (m Message) Text() string {
	msg, err := mail.ReadMessage(strings.NewReader(m.Raw))
	if err != nil {
		return ""
	}
	mt, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil || !strings.HasPrefix(mt, "multipart/") {
		b, _ := io.ReadAll(msg.Body)
		return string(b)
	}
	mr := multipart.NewReader(msg.Body, params["boundary"])
	for {
		p, err := mr.NextPart()
		if err != nil {
			return ""
		}
		if strings.HasPrefix(p.Header.Get("Content-Type"), "text/plain") {
			b, _ := io.ReadAll(quotedprintable.NewReader(p))
			return string(b)
		}
	}
}

// Server accepts connections on a random local port.
type Server struct {
	Host, Port string
	user, pass string

	ln   net.Listener
	mu   sync.Mutex
	msgs []Message
}

// New starts a server that requires AUTH PLAIN with user / pass (empty user =
// no authentication). It stops when the test ends.
func New(t testing.TB, user, pass string) *Server {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	host, port, _ := net.SplitHostPort(ln.Addr().String())
	s := &Server{Host: host, Port: port, user: user, pass: pass, ln: ln}
	go s.serve()
	t.Cleanup(func() { _ = ln.Close() })
	return s
}

// Messages returns a copy of everything delivered so far.
func (s *Server) Messages() []Message {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]Message(nil), s.msgs...)
}

func (s *Server) serve() {
	for {
		c, err := s.ln.Accept()
		if err != nil {
			return
		}
		go s.handle(c)
	}
}

func (s *Server) handle(c net.Conn) {
	defer c.Close()
	r := bufio.NewReader(c)
	say := func(f string, a ...any) { fmt.Fprintf(c, f+"\r\n", a...) }
	say("220 smtptest ready")

	authed := s.user == ""
	var cur Message
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		up := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(up, "EHLO"), strings.HasPrefix(up, "HELO"):
			if s.user != "" {
				say("250-smtptest")
				say("250 AUTH PLAIN")
			} else {
				say("250 smtptest")
			}
		case strings.HasPrefix(up, "AUTH PLAIN"):
			raw, _ := base64.StdEncoding.DecodeString(strings.TrimSpace(line[len("AUTH PLAIN"):]))
			parts := strings.Split(string(raw), "\x00") // authzid, user, pass
			if len(parts) == 3 && parts[1] == s.user && parts[2] == s.pass {
				authed = true
				say("235 2.7.0 authenticated")
			} else {
				say("535 5.7.8 bad credentials")
			}
		case strings.HasPrefix(up, "MAIL FROM:"):
			if !authed {
				say("530 5.7.0 authentication required")
				continue
			}
			cur = Message{From: addr(line[len("MAIL FROM:"):])}
			say("250 ok")
		case strings.HasPrefix(up, "RCPT TO:"):
			cur.To = append(cur.To, addr(line[len("RCPT TO:"):]))
			say("250 ok")
		case up == "DATA":
			say("354 end with <CRLF>.<CRLF>")
			var b strings.Builder
			for {
				l, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if l == ".\r\n" {
					break
				}
				b.WriteString(strings.TrimPrefix(l, ".")) // dot-unstuffing
			}
			cur.Raw = b.String()
			s.mu.Lock()
			s.msgs = append(s.msgs, cur)
			s.mu.Unlock()
			say("250 queued")
		case up == "QUIT":
			say("221 bye")
			return
		default:
			say("250 ok")
		}
	}
}

func addr(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "<")
	if i := strings.Index(s, ">"); i >= 0 {
		s = s[:i]
	}
	return s
}
