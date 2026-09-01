package zcode

import "testing"

func TestProvidersHaveModels(t *testing.T) {
	providers := Providers()
	t.Logf("providers=%d", len(providers))
	for _, p := range providers {
		t.Logf("  %s models=%v", p.ID, p.Models)
		if p.ID != "" && p.Models == nil {
			t.Errorf("provider %s has nil models", p.ID)
		}
	}
}
