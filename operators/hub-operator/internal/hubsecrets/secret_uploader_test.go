package hubsecrets

import (
	"strings"
	"testing"
)

func TestApplyTransformURLPassword(t *testing.T) {
	got, err := applyTransform(TransformURLPassword, []byte("smtps://resend:re_secret_123@smtp.resend.com:465\n"))
	if err != nil || string(got) != "re_secret_123" {
		t.Fatalf("got %q, %v", got, err)
	}
	for _, bad := range []string{"re_just_a_key", "smtps://smtp.resend.com:465", "smtps://resend@smtp.resend.com:465", "smtps://resend:@smtp.resend.com"} {
		if _, err := applyTransform(TransformURLPassword, []byte(bad)); err == nil {
			t.Errorf("%q: expected an error", bad)
		} else if strings.Contains(err.Error(), "re_") {
			t.Errorf("%q: error leaks the value: %v", bad, err)
		}
	}
	if got, _ := applyTransform(TransformNone, []byte("x")); string(got) != "x" {
		t.Errorf("TransformNone changed the value")
	}
	if _, err := applyTransform("rot13", []byte("x")); err == nil {
		t.Errorf("unknown transform accepted")
	}
}

// The owner's SMTP password is stored as the password component of their URL.
func TestSMTPMappingExtractsThePassword(t *testing.T) {
	for _, m := range CLISecretMappings {
		if m.Key == "zitadel_smtp_password" {
			if m.Transform != TransformURLPassword {
				t.Fatalf("zitadel_smtp_password mapping: transform=%q", m.Transform)
			}
			return
		}
	}
	t.Fatal("no CLISecretMapping for zitadel_smtp_password")
}
