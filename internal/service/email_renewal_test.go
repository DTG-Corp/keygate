package service

import (
	"strings"
	"testing"
	"time"

	"github.com/dtg-corp/dsoft/internal/testsupport"
	"github.com/dtg-corp/dsoft/internal/testsupport/testdb"
)

// vnMidnight is a period end whose UTC date (30/11) differs from the
// Vietnamese calendar date (01/12): every decision is UTC, the customer
// reads their own calendar.
var vnMidnight = time.Date(2026, 11, 30, 20, 0, 0, 0, time.UTC)

func newLocalisedEmailService(t *testing.T, language string) *EmailService {
	t.Helper()
	s := newTemplateTestService()
	if language == "" {
		return s
	}
	st := testdb.Store(t)
	s.store = st
	testsupport.SetSetting(t, st.DB, "language", language)
	return s
}

func TestRenderRenewalReminderEnglish(t *testing.T) {
	s := newLocalisedEmailService(t, "")
	subject, body := s.RenderRenewalNotice("Acme App", "Pro", 7, "https://example.com/portal", vnMidnight)
	if !strings.Contains(subject, "Acme App") || !strings.Contains(subject, "7") {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{"Acme App", "Pro", "https://example.com/portal", "01/12/2026"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
	if strings.Contains(body, "gia hạn") {
		t.Fatalf("English site sent Vietnamese copy:\n%s", body)
	}
}

func TestRenderRenewalReminderVietnamese(t *testing.T) {
	s := newLocalisedEmailService(t, "vi")
	subject, body := s.RenderRenewalNotice("Acme App", "Pro", 7, "https://example.com/portal", vnMidnight)
	if !strings.Contains(subject, "gia hạn") {
		t.Fatalf("subject = %q", subject)
	}
	for _, want := range []string{"Acme App", "Pro", "https://example.com/portal", "01/12/2026", "gia hạn"} {
		if !strings.Contains(body, want) {
			t.Fatalf("body missing %q:\n%s", want, body)
		}
	}
}

func TestRenderRenewalDueGraceSuspended(t *testing.T) {
	s := newLocalisedEmailService(t, "")
	dueSubject, due := s.RenderRenewalOverdue("Acme App", "Pro", "https://example.com/portal", vnMidnight)
	graceSubject, grace := s.RenderRenewalGrace("Acme App", "Pro", 3, "https://example.com/portal", vnMidnight)
	suspendedSubject, suspended := s.RenderRenewalSuspension("Acme App", "Pro", "https://example.com/portal", vnMidnight)

	if !strings.Contains(dueSubject, "Acme App") || !strings.Contains(due, "01/12/2026") {
		t.Fatalf("due = %q / %s", dueSubject, due)
	}
	if !strings.Contains(graceSubject, "3") || !strings.Contains(grace, "https://example.com/portal") {
		t.Fatalf("grace = %q / %s", graceSubject, grace)
	}
	if !strings.Contains(suspendedSubject, "Acme App") || !strings.Contains(suspended, "https://example.com/portal") {
		t.Fatalf("suspended = %q / %s", suspendedSubject, suspended)
	}
	// Each rung says something different; a copied body would make the
	// ladder one mail repeated.
	if due == grace || grace == suspended {
		t.Fatal("two dunning rungs rendered the same body")
	}
}

func TestRenewalTemplatesValidateInBothLanguages(t *testing.T) {
	for key, body := range DefaultRenewalTemplates() {
		if err := ValidateTemplate(body); err != nil {
			t.Errorf("default %s template: %v", key, err)
		}
	}
	for key, body := range renewalTemplatesVI {
		if err := ValidateTemplate(body); err != nil {
			t.Errorf("Vietnamese %s template: %v", key, err)
		}
	}
	for _, key := range []string{"renewal_reminder", "renewal_due", "renewal_grace", "renewal_suspended"} {
		if _, ok := DefaultRenewalTemplates()[key]; !ok {
			t.Errorf("DefaultRenewalTemplates is missing %s", key)
		}
		if _, ok := renewalTemplatesVI[key]; !ok {
			t.Errorf("Vietnamese templates are missing %s", key)
		}
	}
}

// An admin's override is still honoured for the new keys, in the
// language the site runs in.
func TestRenewalTemplateOverrideIsUsed(t *testing.T) {
	s := newLocalisedEmailService(t, "en")
	testsupport.SetSetting(t, s.store.DB, "email_template_renewal_due",
		`<html><body><p>OVERRIDE {{.Product}}</p></body></html>`)
	_, body := s.RenderRenewalOverdue("Acme App", "Pro", "https://example.com/portal", vnMidnight)
	if !strings.Contains(body, "OVERRIDE Acme App") {
		t.Fatalf("override ignored:\n%s", body)
	}
}

func TestRenewalReminderFallsBackToThePortalLink(t *testing.T) {
	s := newTemplateTestService()
	_, body := s.RenderRenewalNotice("Acme App", "Pro", 1, "", vnMidnight)
	if !strings.Contains(body, "https://licenses.example.com/portal") {
		t.Fatalf("portal link missing when no pay URL is given:\n%s", body)
	}
}
