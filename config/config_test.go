package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLoadAccounts(t *testing.T) {
	path := filepath.Join(t.TempDir(), "accounts.txt")
	contents := "# comment\naccount-a\n\nsessionKey=account-b; anthropic-device-id=device-b\naccount-a\n"
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatalf("write accounts: %v", err)
	}

	accounts := loadAccounts(path, "account-c", "", "")
	if len(accounts) != 3 {
		t.Fatalf("expected 3 unique accounts, got %d: %#v", len(accounts), accounts)
	}
	if accounts[0].SessionKey != "account-a" || accounts[1].SessionKey != "account-b" || accounts[2].SessionKey != "account-c" {
		t.Fatalf("unexpected accounts: %#v", accounts)
	}
	if accounts[1].Cookie == "" {
		t.Fatal("full Cookie line was not preserved")
	}
}
