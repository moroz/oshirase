package main

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"encoding/hex"
	"errors"
	"fmt"
	"net"
	"net/mail"
	"net/smtp"
	"net/textproto"
	"os"
	"strconv"
	"strings"
	"time"
)

// SMTP: plain message submission with a password, for providers whose
// OAuth grants can do more than send. A Fastmail app password restricted to
// SMTP can only send mail; its OAuth grant can read and delete the whole
// mailbox.

// smtpImplicitTLSPort is the submissions port (RFC 8314): TLS from the
// first byte. Every other port has to offer STARTTLS before the password
// is sent.
const smtpImplicitTLSPort = "465"

// smtpTimeout bounds a whole SMTP exchange when the context has no earlier
// deadline (oshirase auth's probe).
const smtpTimeout = 30 * time.Second

// smtpTransport is how the backend reaches an SMTP server. The zero value
// is the real thing: the system's roots and an ordinary dial. Tests set
// roots to trust their own certificate and dial to reach a fake server;
// nothing the person configures can turn verification off.
type smtpTransport struct {
	roots *x509.CertPool
	dial  func(ctx context.Context, network, address string) (net.Conn, error)
}

// smtpAccount is what an SMTP session needs from the configuration.
type smtpAccount struct {
	Host     string // host:port
	Username string
	Password string
}

func (c *emailConfig) smtpAccount() smtpAccount {
	username := c.Username
	if username == "" {
		username = bareAddress(c.From)
	}
	return smtpAccount{Host: c.SMTPHost, Username: username, Password: c.SMTPPassword}
}

// checkSMTPHost wants host:port, with the port spelt out: it decides
// between TLS from the start and STARTTLS.
func checkSMTPHost(hostPort string) error {
	if hostPort == "" {
		return errors.New("give the SMTP server as host:port, e.g. smtp.fastmail.com:465")
	}
	host, port, err := net.SplitHostPort(hostPort)
	if err != nil || host == "" {
		return fmt.Errorf("%q is not host:port, e.g. smtp.fastmail.com:465 (TLS) or smtp.fastmail.com:587 (STARTTLS)", hostPort)
	}
	if n, err := strconv.Atoi(port); err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%q has no valid port", hostPort)
	}
	return nil
}

// bareAddress is the address part of "Name <a@b>", or text unchanged when
// it does not parse.
func bareAddress(text string) string {
	if address, err := mail.ParseAddress(text); err == nil {
		return address.Address
	}
	return text
}

func (b *emailBackend) sendSMTP(ctx context.Context, message emailMessage) error {
	from, to := bareAddress(b.config.From), bareAddress(b.config.To)
	// Gmail adds a Message-ID; an SMTP server need not, and a message
	// without one looks like spam to some filters.
	message.Headers = append([][2]string{{"Message-ID", newMessageID(from)}}, message.Headers...)
	raw := buildRFC5322(from, to, message, b.clock())
	return b.smtpNet.session(ctx, b.config.smtpAccount(), func(client *smtp.Client, fail func(string, error) error) error {
		if err := client.Mail(from); err != nil {
			return fail("the sender "+from, err)
		}
		if err := client.Rcpt(to); err != nil {
			return fail("the recipient "+to, err)
		}
		data, err := client.Data()
		if err != nil {
			return fail("the message", err)
		}
		if _, err := data.Write(raw); err != nil {
			return fail("the message", err)
		}
		if err := data.Close(); err != nil {
			return fail("the message", err)
		}
		return nil
	})
}

// probe logs in and out again without sending anything: oshirase auth's
// check of a password before it is saved.
func (t smtpTransport) probe(ctx context.Context, account smtpAccount) error {
	return t.session(ctx, account, nil)
}

// newMessageID is <random@domain of from>.
func newMessageID(from string) string {
	buffer := make([]byte, 16)
	if _, err := rand.Read(buffer); err != nil {
		panic(err) // crypto/rand does not fail on supported platforms
	}
	domain := "oshirase.invalid"
	if at := strings.LastIndex(from, "@"); at >= 0 && at < len(from)-1 {
		domain = from[at+1:]
	}
	return "<" + hex.EncodeToString(buffer) + "@" + domain + ">"
}

// session connects, makes sure the connection is encrypted, logs in, runs
// then (when not nil) and says goodbye. Every error names the server and
// never holds the password: net/smtp's errors are the server's replies,
// not the commands sent.
func (t smtpTransport) session(ctx context.Context, account smtpAccount, then func(*smtp.Client, func(string, error) error) error) error {
	ctx, cancel := context.WithTimeout(ctx, smtpTimeout)
	defer cancel()
	host, port, err := net.SplitHostPort(account.Host)
	if err != nil {
		return fmt.Errorf("smtp_host: %w", checkSMTPHost(account.Host))
	}
	fail := func(stage string, err error) error {
		return smtpFailure(ctx, account.Host, stage, err)
	}

	dial := t.dial
	if dial == nil {
		dial = (&net.Dialer{}).DialContext
	}
	conn, err := dial(ctx, "tcp", account.Host)
	if err != nil {
		return fail("connecting", err)
	}
	defer conn.Close()
	// The deadline holds for everything below; a context cancelled early
	// (the caller gone) ends it at once.
	if deadline, ok := ctx.Deadline(); ok {
		conn.SetDeadline(deadline)
	}
	stop := context.AfterFunc(ctx, func() { conn.SetDeadline(time.Unix(1, 0)) })
	defer stop()

	tlsConfig := &tls.Config{ServerName: host, RootCAs: t.roots, MinVersion: tls.VersionTLS12}
	var client *smtp.Client
	if port == smtpImplicitTLSPort {
		secure := tls.Client(conn, tlsConfig)
		if err := secure.HandshakeContext(ctx); err != nil {
			return fail("TLS", err)
		}
		if client, err = smtp.NewClient(secure, host); err != nil {
			return fail("the greeting", err)
		}
	} else {
		if client, err = smtp.NewClient(conn, host); err != nil {
			return fail("the greeting", err)
		}
		if err := client.Hello("localhost"); err != nil {
			return fail("EHLO", err)
		}
		if ok, _ := client.Extension("STARTTLS"); !ok {
			client.Close()
			return fmt.Errorf("the SMTP server %s does not offer STARTTLS, so the password would go unencrypted; not logging in (port %s is TLS from the start)", account.Host, smtpImplicitTLSPort)
		}
		if err := client.StartTLS(tlsConfig); err != nil {
			return fail("STARTTLS", err)
		}
	}
	defer client.Close()

	// PlainAuth refuses to send the password over a connection that is not
	// TLS; both paths above have made it TLS by now.
	if err := client.Auth(smtp.PlainAuth("", account.Username, account.Password, host)); err != nil {
		return fail("the login", err)
	}
	if then != nil {
		if err := then(client, fail); err != nil {
			return err
		}
	}
	if err := client.Quit(); err != nil {
		return fail("QUIT", err)
	}
	return nil
}

// smtpFailure turns what went wrong at stage into what the person can do
// about it.
func smtpFailure(ctx context.Context, hostPort, stage string, err error) error {
	var reply *textproto.Error
	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	var invalid x509.CertificateInvalidError
	switch {
	case errors.As(err, &reply) && reply.Code == 535:
		return fmt.Errorf("the SMTP server %s refused the password (535 %s); make a new app password and run oshirase auth smtp",
			hostPort, truncate(reply.Msg, 200))
	case errors.As(err, &reply):
		return fmt.Errorf("the SMTP server %s refused %s (%d %s)", hostPort, stage, reply.Code, truncate(reply.Msg, 200))
	case ctx.Err() != nil || errors.Is(err, os.ErrDeadlineExceeded):
		cause := ctx.Err()
		if cause == nil {
			cause = context.DeadlineExceeded
		}
		return fmt.Errorf("the SMTP server %s did not answer in time (at %s): %w", hostPort, stage, cause)
	case errors.As(err, &unknownAuthority), errors.As(err, &hostname), errors.As(err, &invalid):
		return fmt.Errorf("the certificate of the SMTP server %s does not verify: %v", hostPort, err)
	case stage == "connecting":
		return fmt.Errorf("connecting to the SMTP server %s: %v", hostPort, err)
	default:
		return fmt.Errorf("the SMTP server %s, at %s: %v", hostPort, stage, err)
	}
}
