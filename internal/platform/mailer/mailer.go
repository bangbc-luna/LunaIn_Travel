// Package mailer sends transactional email through a pluggable Mailer.
package mailer

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"mime"
	"net"
	"net/mail"
	"net/smtp"
	"strings"
	"sync"
	"time"
)

// Message is a plain-text email.
type Message struct {
	To      string `json:"to"`
	Subject string `json:"subject"`
	Text    string `json:"text"`
}

// Mailer delivers a message.
type Mailer interface {
	Send(ctx context.Context, m Message) error
}

// LogMailer only logs messages (dev/test).
type LogMailer struct {
	Logger *slog.Logger
}

func (l LogMailer) Send(ctx context.Context, m Message) error {
	logger := l.Logger
	if logger == nil {
		logger = slog.Default()
	}
	logger.InfoContext(ctx, "email (not sent, log mailer)", "to", m.To, "subject", m.Subject, "text", m.Text)
	return nil
}

// Recorder keeps sent messages in memory for tests. The zero value is usable.
type Recorder struct {
	mu   sync.Mutex
	msgs []Message
}

func (r *Recorder) Send(_ context.Context, m Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.msgs = append(r.msgs, m)
	return nil
}

// Messages returns a copy of the recorded messages.
func (r *Recorder) Messages() []Message {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]Message(nil), r.msgs...)
}

const defaultSMTPTimeout = 30 * time.Second

// SMTPMailer sends via net/smtp, using STARTTLS when the server offers it.
type SMTPMailer struct {
	Host     string
	Port     int
	User     string
	Password string
	From     string
}

// BuildMessage renders an RFC 5322 message with a UTF-8 text/plain body.
func BuildMessage(from string, m Message) []byte {
	var b strings.Builder
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + m.To + "\r\n")
	b.WriteString("Subject: " + mime.QEncoding.Encode("UTF-8", m.Subject) + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(m.Text)
	return []byte(b.String())
}

func (s SMTPMailer) Send(ctx context.Context, m Message) error {
	if strings.ContainsAny(m.To, "\r\n") || strings.ContainsAny(m.Subject, "\r\n") {
		return fmt.Errorf("mailer: invalid header characters")
	}
	from, err := mail.ParseAddress(s.From)
	if err != nil {
		return fmt.Errorf("mailer: parse from: %w", err)
	}
	to, err := mail.ParseAddress(m.To)
	if err != nil {
		return fmt.Errorf("mailer: parse to: %w", err)
	}

	addr := net.JoinHostPort(s.Host, fmt.Sprint(s.Port))
	var d net.Dialer
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("mailer: dial: %w", err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(defaultSMTPTimeout)
	}
	_ = conn.SetDeadline(deadline)
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-done:
		}
	}()
	// wrap converts I/O failures caused by cancellation into ctx errors.
	wrap := func(op string, err error) error {
		if cerr := ctx.Err(); cerr != nil {
			return fmt.Errorf("mailer: %s: %w", op, cerr)
		}
		return fmt.Errorf("mailer: %s: %w", op, err)
	}
	c, err := smtp.NewClient(conn, s.Host)
	if err != nil {
		_ = conn.Close()
		return wrap("smtp client", err)
	}
	defer c.Close()

	if ok, _ := c.Extension("STARTTLS"); ok {
		if err := c.StartTLS(&tls.Config{ServerName: s.Host, MinVersion: tls.VersionTLS12}); err != nil {
			return wrap("starttls", err)
		}
	}
	if s.User != "" {
		if err := c.Auth(smtp.PlainAuth("", s.User, s.Password, s.Host)); err != nil {
			return wrap("auth", err)
		}
	}
	if err := c.Mail(from.Address); err != nil {
		return wrap("mail from", err)
	}
	if err := c.Rcpt(to.Address); err != nil {
		return wrap("rcpt", err)
	}
	w, err := c.Data()
	if err != nil {
		return wrap("data", err)
	}
	if _, err := w.Write(BuildMessage(s.From, m)); err != nil {
		return wrap("write", err)
	}
	if err := w.Close(); err != nil {
		return wrap("close data", err)
	}
	if err := c.Quit(); err != nil {
		return wrap("quit", err)
	}
	return nil
}
