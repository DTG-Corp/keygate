// Package branding provides project identity constants used across the
// application for API responses, HTML meta tags, HTTP headers, and email
// footers.
package branding

// Core identity — referenced by handlers, middleware, and templates.
var (
	Project = Product
	Domain  = "dtgsoft.vn"
	URL     = ProductURL
	Tagline = pwrd + " " + Product
)

// HTTP header values.
const HeaderKey = "X-Powered-By"

// Email footer (HTML).
var EmailFooter = `<hr style="border:none;border-top:1px solid #e5e7eb;margin:32px 0 16px"/>` +
	`<p style="color:#9ca3af;font-size:11px;text-align:center;">` + pwrd +
	` <a href="` + ProductURL + `" style="color:#9ca3af;">` +
	Product + `</a></p>`

const pwrd = "Powered by"
