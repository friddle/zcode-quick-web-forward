package main

import "testing"

func TestLoadZcodeUserInfoLocal(t *testing.T) {
	if testing.Short() {
		t.Skip("local env")
	}
	ui := loadZcodeUserInfo()
	if ui == nil {
		t.Log("no signed-in credentials on this machine (ok)")
		return
	}
	t.Logf("username=%q displayName=%q email=%q", ui.Name, ui.Email, ui.Avatar)
	if ui.Name == "" {
		t.Fatal("username empty")
	}
}
