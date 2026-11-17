package branding_test

import (
	"testing"

	"github.com/dtg-corp/dsoft/internal/branding"
)

func TestBrandingValues(t *testing.T) {
	if branding.Project != "DSoft" {
		t.Errorf("Project = %q", branding.Project)
	}
	if branding.Domain != "dtgsoft.vn" {
		t.Errorf("Domain = %q", branding.Domain)
	}
	if branding.URL != "https://github.com/dtg-corp/dsoft" {
		t.Errorf("URL = %q", branding.URL)
	}
	if branding.Tagline != "Powered by DSoft" {
		t.Errorf("Tagline = %q", branding.Tagline)
	}
}
