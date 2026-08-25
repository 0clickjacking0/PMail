package send

import (
	stdcontext "context"
	"errors"
	"fmt"
	"github.com/Jinnrry/pmail/dto/parsemail"
	pmailcontext "github.com/Jinnrry/pmail/utils/context"
	"net"
	"net/textproto"
	"strings"
	"testing"
)

func TestDoSendUsesSinglePlaintextPort25Delivery(t *testing.T) {
	tests := []struct {
		name             string
		recipient        string
		mx               []*net.MX
		lookupErr        error
		sendErr          error
		wantLookupDomain string
		wantAddr         string
		wantErrKey       string
	}{
		{
			name:             "MX delivery",
			recipient:        "recipient@example.net",
			mx:               []*net.MX{{Host: "mx.example.net."}},
			wantLookupDomain: "example.net",
			wantAddr:         "mx.example.net.:25",
		},
		{
			name:             "failure is returned without fallback",
			recipient:        "recipient@example.net",
			mx:               []*net.MX{{Host: "mx.example.net."}},
			sendErr:          errors.New("connection refused"),
			wantLookupDomain: "example.net",
			wantAddr:         "mx.example.net.:25",
			wantErrKey:       "example.net",
		},
		{
			name:             "DNS fallback",
			recipient:        "recipient@missing.example",
			lookupErr:        &net.DNSError{Err: "no such host", Name: "missing.example", IsNotFound: true},
			wantLookupDomain: "missing.example",
			wantAddr:         "smtp.missing.example:25",
		},
		{
			name:      "test domain",
			recipient: "recipient@test.domain",
			wantAddr:  "127.0.0.1:25",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			lookupCalls := 0
			lookupMX := func(domain string) ([]*net.MX, error) {
				lookupCalls++
				if domain != tt.wantLookupDomain {
					t.Errorf("lookupMX() domain = %q, want %q", domain, tt.wantLookupDomain)
				}
				return tt.mx, tt.lookupErr
			}

			sendCalls := 0
			sendMail := func(addr, from, fromDomain string, to []string, data []byte) error {
				sendCalls++
				if addr != tt.wantAddr {
					t.Errorf("sendMail() addr = %q, want %q", addr, tt.wantAddr)
				}
				return tt.sendErr
			}

			ctx := &pmailcontext.Context{Context: stdcontext.Background()}
			err, errMap := doSendWith(
				ctx,
				"example.com",
				[]byte("Subject: test\r\n\r\nbody\r\n"),
				[]*parsemail.User{{EmailAddress: tt.recipient}},
				"sender@example.com",
				lookupMX,
				sendMail,
			)

			wantLookupCalls := 1
			if tt.wantLookupDomain == "" {
				wantLookupCalls = 0
			}
			if lookupCalls != wantLookupCalls {
				t.Errorf("lookupMX() calls = %d, want %d", lookupCalls, wantLookupCalls)
			}
			if sendCalls != 1 {
				t.Fatalf("sendMail() calls = %d, want 1", sendCalls)
			}

			if tt.wantErrKey != "" {
				if err == nil || !strings.Contains(err.Error(), tt.recipient) {
					t.Fatalf("doSend() error = %v, want failed recipient", err)
				}
				if !errors.Is(errMap[tt.wantErrKey], tt.sendErr) {
					t.Fatalf("doSend() domain error = %v, want %v", errMap[tt.wantErrKey], tt.sendErr)
				}
				return
			}

			if err != nil {
				t.Fatalf("doSend() error = %v, want nil", err)
			}
			if len(errMap) != 0 {
				t.Fatalf("doSend() error map = %v, want empty", errMap)
			}
		})
	}
}

func TestDoSendKeepsFailuresFromMixedMXResolution(t *testing.T) {
	lookupCalls := 0
	lookupErr := &net.DNSError{Err: "server misbehaving", Name: "example.net", IsTemporary: true}
	lookupMX := func(domain string) ([]*net.MX, error) {
		lookupCalls++
		if lookupCalls == 1 {
			return []*net.MX{{Host: "mx.example.net."}}, nil
		}
		return nil, lookupErr
	}

	permanentErr := &textproto.Error{Code: 550, Msg: "mailbox unavailable"}
	temporaryErr := &net.OpError{Op: "dial", Net: "tcp", Err: errors.New("connection refused")}
	sendMail := func(addr, from, fromDomain string, to []string, data []byte) error {
		switch addr {
		case "mx.example.net.:25":
			return permanentErr
		case "smtp.example.net:25":
			return temporaryErr
		default:
			return fmt.Errorf("unexpected SMTP address: %s", addr)
		}
	}

	ctx := &pmailcontext.Context{Context: stdcontext.Background()}
	err, errMap := doSendWith(
		ctx,
		"example.com",
		[]byte("Subject: test\r\n\r\nbody\r\n"),
		[]*parsemail.User{
			{EmailAddress: "first@example.net"},
			{EmailAddress: "second@example.net"},
		},
		"sender@example.com",
		lookupMX,
		sendMail,
	)

	if err == nil {
		t.Fatal("doSendWith() error = nil, want delivery failure")
	}
	if len(errMap) != 2 {
		t.Fatalf("doSendWith() error map = %v, want two independent delivery groups", errMap)
	}
	if !errors.Is(errMap["example.net"], permanentErr) {
		t.Errorf("MX delivery error = %v, want %v", errMap["example.net"], permanentErr)
	}
	var fallbackErr *temporaryMXFallbackError
	if !errors.As(errMap["smtp.example.net"], &fallbackErr) {
		t.Errorf("fallback delivery error = %v, want *temporaryMXFallbackError", errMap["smtp.example.net"])
	}
}

func TestIsPermanentSMTPResponse(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "nil"},
		{name: "network error", err: errors.New("connection refused")},
		{name: "temporary SMTP response", err: &textproto.Error{Code: 451, Msg: "try later"}},
		{name: "wrapped temporary SMTP response", err: fmt.Errorf("delivery: %w", &textproto.Error{Code: 421, Msg: "service unavailable"})},
		{name: "permanent SMTP response", err: &textproto.Error{Code: 550, Msg: "mailbox unavailable"}, want: true},
		{name: "wrapped permanent SMTP response", err: fmt.Errorf("delivery: %w", &textproto.Error{Code: 554, Msg: "transaction failed"}), want: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isPermanentSMTPResponse(tt.err); got != tt.want {
				t.Fatalf("isPermanentSMTPResponse() = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestDeliveryFailureCausePreservesTemporaryMXError(t *testing.T) {
	tests := []struct {
		name      string
		lookupErr *net.DNSError
	}{
		{
			name:      "SERVFAIL before fallback NXDOMAIN",
			lookupErr: &net.DNSError{Err: "server misbehaving", Name: "example.com", IsTemporary: true},
		},
		{
			name:      "timeout before fallback NXDOMAIN",
			lookupErr: &net.DNSError{Err: "i/o timeout", Name: "example.com", IsTimeout: true},
		},
	}

	fallbackErr := &net.OpError{
		Op:  "dial",
		Net: "tcp",
		Err: &net.DNSError{Err: "no such host", Name: "smtp.example.com", IsNotFound: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			err := deliveryFailureCause(tt.lookupErr, fallbackErr)

			var dnsErr *net.DNSError
			if !errors.As(err, &dnsErr) {
				t.Fatalf("deliveryFailureCause() = %T, want wrapped DNS error", err)
			}
			if dnsErr.IsNotFound {
				t.Fatalf("deliveryFailureCause() selected fallback NXDOMAIN: %v", dnsErr)
			}
			if !dnsErr.IsTemporary && !dnsErr.IsTimeout {
				t.Fatalf("deliveryFailureCause() selected non-temporary DNS error: %v", dnsErr)
			}
		})
	}
}

func TestDeliveryFailureCauseKeepsExplicitSMTPRejection(t *testing.T) {
	lookupErr := &net.DNSError{Err: "server misbehaving", Name: "example.com", IsTemporary: true}
	rejection := &textproto.Error{Code: 550, Msg: "mailbox unavailable"}

	if got := deliveryFailureCause(lookupErr, rejection); got != rejection {
		t.Fatalf("deliveryFailureCause() = %v, want explicit SMTP rejection %v", got, rejection)
	}
}
