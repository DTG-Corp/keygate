package branding_test

import (
	"testing"

	"github.com/dtg-corp/dsoft/internal/branding"
)

func TestBrandingValues(t *testing.T) {
	if branding.Project != "Keygate" {
		t.Errorf("Project = %q", branding.Project)
	}
	if branding.Domain != "keygate.app" {
		t.Errorf("Domain = %q", branding.Domain)
	}
	if branding.URL != "https://keygate.app" {
		t.Errorf("URL = %q", branding.URL)
	}
	if branding.Tagline != "Powered by Keygate" {
		t.Errorf("Tagline = %q", branding.Tagline)
	}
}
