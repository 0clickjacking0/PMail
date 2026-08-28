package smtp_server

import (
	"testing"
)

// TestSessionResetClearsState 验证 Reset() 清除 From 和 To 状态。
// 在同一条 SMTP 连接上连续发送多封邮件时，每封邮件之间会调用 RSET，
// 如果 Session.Reset() 不清除上一封的 From/To，会导致下一封邮件
// 的收件人列表残留上一封的数据（邮件串发）。
func TestSessionResetClearsState(t *testing.T) {
	s := &Session{
		From: "sender@example.com",
		To:   []string{"rcpt1@example.com", "rcpt2@example.com"},
	}

	s.Reset()

	if s.From != "" {
		t.Errorf("Reset() From = %q, want empty", s.From)
	}
	if s.To != nil {
		t.Errorf("Reset() To = %v, want nil", s.To)
	}
}

// TestSessionResetIdempotent 验证多次调用 Reset() 不产生异常。
func TestSessionResetIdempotent(t *testing.T) {
	s := &Session{}
	s.Reset()
	s.Reset()
	if s.From != "" || s.To != nil {
		t.Fatal("Reset() on empty session should remain empty")
	}
}
