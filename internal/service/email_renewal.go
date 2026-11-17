package service

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// This file holds the two ladders the billing engine mails: the renewal
// reminders before a period ends and the dunning notices after it. They
// are twins of the Stripe-era templates but carry what a Vietnamese bank
// transfer needs — the amount is already in the order, so the mail only
// has to say what happens and where to pay.
//
// Every body is rendered with existing template variables (Product, Plan,
// DaysLeft, ExpiresAt, PortalURL, SiteName) so an admin can override it
// through the usual email_template_<key> setting without a new variable
// having to be taught to ValidateTemplate.
//
// The site language setting picks the copy: one body per key is stored,
// so the language decides which default is used when the admin has not
// overridden it. Vietnamese is the default for a site whose `language`
// setting starts with "vi"; everything else gets English.

// ictOffset is UTC+07:00, the calendar every Vietnamese customer reads.
// A fixed offset keeps the service free of a tzdata dependency; the
// decision logic never uses it.
const ictOffset = 7 * 3600

// siteLanguage reports the language the site is configured in ("vi" or
// "en"). A missing setting or an unreadable store is English, which is
// what phase 1a always sent.
func (s *EmailService) siteLanguage() string {
	if s.store == nil {
		return "en"
	}
	v, err := s.store.GetSetting(context.Background(), "language")
	if err != nil {
		return "en"
	}
	if strings.HasPrefix(strings.ToLower(strings.TrimSpace(v)), "vi") {
		return "vi"
	}
	return "en"
}

// renewalDate renders a period end on the Vietnamese calendar.
func renewalDate(t time.Time) string {
	return t.In(time.FixedZone("ICT", ictOffset)).Format("02/01/2006")
}

func daysWord(days int) string {
	if days == 1 {
		return "day"
	}
	return "days"
}

// renewalTemplate picks the default body for a key in the site language.
func (s *EmailService) renewalTemplate(key string) string {
	if s.siteLanguage() == "vi" {
		if body, ok := renewalTemplatesVI[key]; ok {
			return body
		}
	}
	return DefaultRenewalTemplates()[key]
}

// renderRenewal renders one of the four bodies with its variables. The
// pay link is the portal (phase 1b has no server-side payment page), and
// it is passed as PortalURL so the same variable keeps working in an
// admin's override.
func (s *EmailService) renderRenewal(key string, data map[string]any) string {
	if payURL, ok := data["payURL"].(string); ok {
		delete(data, "payURL")
		if payURL != "" {
			data["PortalURL"] = payURL
		}
	}
	return s.renderEmail(key, s.renewalTemplate(key), s.templateData(data))
}

// RenderRenewalNotice builds the pre-expiry rung: the plan, the day it
// renews and the link to pay. (The Stripe-era RenderRenewalReminder in
// email.go is a different, licence-only notice and keeps its name.)
func (s *EmailService) RenderRenewalNotice(productName, planName string, daysLeft int, payURL string, periodEnd time.Time) (subject, body string) {
	if s.siteLanguage() == "vi" {
		subject = fmt.Sprintf("%s sẽ gia hạn sau %d ngày", productName, daysLeft)
	} else {
		subject = fmt.Sprintf("%s subscription renews in %d %s", productName, daysLeft, daysWord(daysLeft))
	}
	body = s.renderRenewal("renewal_reminder", map[string]any{
		"Product": productName, "Plan": planName, "DaysLeft": daysLeft,
		"ExpiresAt": renewalDate(periodEnd), "payURL": payURL,
	})
	return subject, body
}

// RenderRenewalOverdue builds the first dunning step: the period ended
// and the licence is now past due.
func (s *EmailService) RenderRenewalOverdue(productName, planName, payURL string, periodEnd time.Time) (subject, body string) {
	if s.siteLanguage() == "vi" {
		subject = "Đã đến hạn thanh toán " + productName
	} else {
		subject = productName + " payment is due"
	}
	body = s.renderRenewal("renewal_due", map[string]any{
		"Product": productName, "Plan": planName,
		"ExpiresAt": renewalDate(periodEnd), "payURL": payURL,
	})
	return subject, body
}

// RenderRenewalGrace builds the grace-period rung: access still works,
// but only for the days counted here.
func (s *EmailService) RenderRenewalGrace(productName, planName string, daysLeft int, payURL string, periodEnd time.Time) (subject, body string) {
	if s.siteLanguage() == "vi" {
		subject = fmt.Sprintf("%s: còn %d ngày để gia hạn", productName, daysLeft)
	} else {
		subject = fmt.Sprintf("%s: %d %s left to renew", productName, daysLeft, daysWord(daysLeft))
	}
	body = s.renderRenewal("renewal_grace", map[string]any{
		"Product": productName, "Plan": planName, "DaysLeft": daysLeft,
		"ExpiresAt": renewalDate(periodEnd), "payURL": payURL,
	})
	return subject, body
}

// RenderRenewalSuspension builds the last rung: the licence is suspended
// until the outstanding period is paid.
func (s *EmailService) RenderRenewalSuspension(productName, planName, payURL string, periodEnd time.Time) (subject, body string) {
	if s.siteLanguage() == "vi" {
		subject = "Giấy phép " + productName + " đã bị tạm ngưng"
	} else {
		subject = productName + " access suspended"
	}
	body = s.renderRenewal("renewal_suspended", map[string]any{
		"Product": productName, "Plan": planName,
		"ExpiresAt": renewalDate(periodEnd), "payURL": payURL,
	})
	return subject, body
}

// SendRenewalReminder mails the pre-expiry reminder. It is the
// fire-and-forget twin of RenderRenewalNotice; the worker uses the
// renderer plus the durable queue instead, so a lost SMTP session cannot
// swallow a rung.
func (s *EmailService) SendRenewalReminder(to, productName, planName string, daysLeft int, payURL string, periodEnd time.Time) {
	subject, body := s.RenderRenewalNotice(productName, planName, daysLeft, payURL, periodEnd)
	go func() {
		if err := s.Send(to, subject, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

// SendRenewalDue mails the first dunning step.
func (s *EmailService) SendRenewalDue(to, productName, planName, payURL string, periodEnd time.Time) {
	subject, body := s.RenderRenewalOverdue(productName, planName, payURL, periodEnd)
	go func() {
		if err := s.Send(to, subject, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

// SendRenewalGrace mails the grace-period rung.
func (s *EmailService) SendRenewalGrace(to, productName, planName string, daysLeft int, payURL string, periodEnd time.Time) {
	subject, body := s.RenderRenewalGrace(productName, planName, daysLeft, payURL, periodEnd)
	go func() {
		if err := s.Send(to, subject, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

// SendRenewalSuspended mails the suspension notice.
func (s *EmailService) SendRenewalSuspended(to, productName, planName, payURL string, periodEnd time.Time) {
	subject, body := s.RenderRenewalSuspension(productName, planName, payURL, periodEnd)
	go func() {
		if err := s.Send(to, subject, body); err != nil {
			s.logger.Error("email delivery failed", "to", to, "error", err)
		}
	}()
}

// DefaultRenewalTemplates is the English half of the four bodies. It is
// the map the Lead merges into DefaultTemplates() in email.go, which is
// shared; keeping it here is what lets this module add its keys without
// editing that file.
func DefaultRenewalTemplates() map[string]string {
	return map[string]string{
		"renewal_reminder":  tmplRenewalReminderEN,
		"renewal_due":       tmplRenewalDueEN,
		"renewal_grace":     tmplRenewalGraceEN,
		"renewal_suspended": tmplRenewalSuspendedEN,
	}
}

var renewalTemplatesVI = map[string]string{
	"renewal_reminder":  tmplRenewalReminderVI,
	"renewal_due":       tmplRenewalDueVI,
	"renewal_grace":     tmplRenewalGraceVI,
	"renewal_suspended": tmplRenewalSuspendedVI,
}

const renewalBoxStyle = `font-family: -apple-system, sans-serif; max-width: 600px; margin: 0 auto; padding: 20px;`

const tmplRenewalReminderEN = `<!DOCTYPE html>
<html><body style="` + renewalBoxStyle + `">
<h2>Your {{.Product}} subscription renews in {{.DaysLeft}} days</h2>
<p>Your <strong>{{.Plan}}</strong> plan for {{.Product}} renews on <strong>{{.ExpiresAt}}</strong>.</p>
<p>Pay now to keep your licence active without interruption:</p>
<p><a href="{{.PortalURL}}" style="background:#111;color:#fff;padding:12px 20px;border-radius:6px;text-decoration:none;">Renew now</a></p>
<p>If the renewal is not paid, your licence stays active until the period ends, then moves to past due and keeps a short grace period.</p>
<p>— {{.SiteName}}</p>
</body></html>`

const tmplRenewalDueEN = `<!DOCTYPE html>
<html><body style="` + renewalBoxStyle + `">
<h2 style="color:#d97706;">{{.Product}} payment is due</h2>
<p>Your <strong>{{.Plan}}</strong> plan for {{.Product}} reached the end of its billing period on <strong>{{.ExpiresAt}}</strong>.</p>
<p>Your licence is now past due. You keep access during the grace period — renew now to avoid interruption:</p>
<p><a href="{{.PortalURL}}" style="background:#111;color:#fff;padding:12px 20px;border-radius:6px;text-decoration:none;">Pay now</a></p>
<p>— {{.SiteName}}</p>
</body></html>`

const tmplRenewalGraceEN = `<!DOCTYPE html>
<html><body style="` + renewalBoxStyle + `">
<h2 style="color:#d97706;">{{.Product}}: {{.DaysLeft}} days left to renew</h2>
<p>Your <strong>{{.Plan}}</strong> plan for {{.Product}} has been past due since <strong>{{.ExpiresAt}}</strong>.</p>
<p>Renew within {{.DaysLeft}} days to keep your licence active. When the grace period ends, the licence is suspended.</p>
<p><a href="{{.PortalURL}}" style="background:#111;color:#fff;padding:12px 20px;border-radius:6px;text-decoration:none;">Renew now</a></p>
<p>— {{.SiteName}}</p>
</body></html>`

const tmplRenewalSuspendedEN = `<!DOCTYPE html>
<html><body style="` + renewalBoxStyle + `">
<h2 style="color:#dc2626;">{{.Product}} access suspended</h2>
<p>Your <strong>{{.Plan}}</strong> plan for {{.Product}} is suspended because the payment for the period ending <strong>{{.ExpiresAt}}</strong> was not received.</p>
<p>Renew now and your licence is reactivated immediately:</p>
<p><a href="{{.PortalURL}}" style="background:#111;color:#fff;padding:12px 20px;border-radius:6px;text-decoration:none;">Pay and reactivate</a></p>
<p>— {{.SiteName}}</p>
</body></html>`

const tmplRenewalReminderVI = `<!DOCTYPE html>
<html><body style="` + renewalBoxStyle + `">
<h2>Gói {{.Product}} của bạn sẽ gia hạn sau {{.DaysLeft}} ngày</h2>
<p>Gói <strong>{{.Plan}}</strong> cho {{.Product}} sẽ đến hạn vào <strong>{{.ExpiresAt}}</strong>.</p>
<p>Thanh toán ngay để giấy phép của bạn tiếp tục hoạt động không gián đoạn:</p>
<p><a href="{{.PortalURL}}" style="background:#111;color:#fff;padding:12px 20px;border-radius:6px;text-decoration:none;">Gia hạn ngay</a></p>
<p>Nếu chưa gia hạn, giấy phép vẫn hoạt động đến hết kỳ hiện tại, sau đó chuyển sang quá hạn và còn một thời gian ân hạn ngắn.</p>
<p>— {{.SiteName}}</p>
</body></html>`

const tmplRenewalDueVI = `<!DOCTYPE html>
<html><body style="` + renewalBoxStyle + `">
<h2 style="color:#d97706;">Đã đến hạn thanh toán {{.Product}}</h2>
<p>Gói <strong>{{.Plan}}</strong> cho {{.Product}} đã hết kỳ thanh toán vào <strong>{{.ExpiresAt}}</strong>.</p>
<p>Giấy phép của bạn đang ở trạng thái quá hạn. Bạn vẫn sử dụng được trong thời gian ân hạn — hãy gia hạn ngay để không bị gián đoạn:</p>
<p><a href="{{.PortalURL}}" style="background:#111;color:#fff;padding:12px 20px;border-radius:6px;text-decoration:none;">Thanh toán ngay</a></p>
<p>— {{.SiteName}}</p>
</body></html>`

const tmplRenewalGraceVI = `<!DOCTYPE html>
<html><body style="` + renewalBoxStyle + `">
<h2 style="color:#d97706;">{{.Product}}: còn {{.DaysLeft}} ngày để gia hạn</h2>
<p>Gói <strong>{{.Plan}}</strong> cho {{.Product}} đã quá hạn từ <strong>{{.ExpiresAt}}</strong>.</p>
<p>Gia hạn trong {{.DaysLeft}} ngày để giấy phép tiếp tục hoạt động. Hết thời gian ân hạn, giấy phép sẽ bị tạm ngưng.</p>
<p><a href="{{.PortalURL}}" style="background:#111;color:#fff;padding:12px 20px;border-radius:6px;text-decoration:none;">Gia hạn ngay</a></p>
<p>— {{.SiteName}}</p>
</body></html>`

const tmplRenewalSuspendedVI = `<!DOCTYPE html>
<html><body style="` + renewalBoxStyle + `">
<h2 style="color:#dc2626;">Giấy phép {{.Product}} đã bị tạm ngưng</h2>
<p>Gói <strong>{{.Plan}}</strong> cho {{.Product}} bị tạm ngưng vì chưa nhận được thanh toán cho kỳ kết thúc ngày <strong>{{.ExpiresAt}}</strong>.</p>
<p>Gia hạn ngay để giấy phép được kích hoạt lại:</p>
<p><a href="{{.PortalURL}}" style="background:#111;color:#fff;padding:12px 20px;border-radius:6px;text-decoration:none;">Thanh toán và kích hoạt lại</a></p>
<p>— {{.SiteName}}</p>
</body></html>`
