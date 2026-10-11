package crypt

import (
	"strings"
	"testing"

	"golang.org/x/crypto/bcrypt"
)

func TestHashPasswordProducesBcrypt(t *testing.T) {
	hash, err := HashPassword("Str0ng-Passw0rd!")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("expected bcrypt hash prefix $2, got %q", hash)
	}
	if len(hash) > 128 {
		t.Fatalf("hash length %d exceeds user.password column size 128", len(hash))
	}
	ok, needRehash := VerifyPassword(hash, "Str0ng-Passw0rd!")
	if !ok || needRehash {
		t.Fatalf("VerifyPassword() = (%v, %v), want (true, false)", ok, needRehash)
	}
	ok, _ = VerifyPassword(hash, "wrong-password")
	if ok {
		t.Fatal("VerifyPassword() accepted a wrong password")
	}
}

func TestHashPasswordIsSalted(t *testing.T) {
	a, err := HashPassword("Str0ng-Passw0rd!")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	b, err := HashPassword("Str0ng-Passw0rd!")
	if err != nil {
		t.Fatalf("HashPassword() error = %v", err)
	}
	if a == b {
		t.Fatal("two hashes of the same password are identical: bcrypt salt is not being used")
	}
}

func TestVerifyPasswordLegacyMD5Upgrades(t *testing.T) {
	// e10adc3949ba59abbe56e057f20f883e 是旧版种子账号 root 的默认口令 123456 的 MD5。
	legacy := LegacyMD5("123456")
	if legacy != "e10adc3949ba59abbe56e057f20f883e" {
		t.Fatalf("LegacyMD5(123456) = %s, unexpected value", legacy)
	}
	ok, needRehash := VerifyPassword(legacy, "123456")
	if !ok {
		t.Fatal("legacy MD5 password was rejected: existing accounts would be locked out")
	}
	if !needRehash {
		t.Fatal("needRehash must be true for legacy MD5 hashes so callers can upgrade them")
	}
	if ok, _ := VerifyPassword(legacy, "123457"); ok {
		t.Fatal("legacy MD5 verification accepted a wrong password")
	}
}

func TestValidatePasswordStrength(t *testing.T) {
	cases := []struct {
		name     string
		password string
		wantErr  error
	}{
		{"empty", "", ErrEmptyPassword},
		{"too short", "Ab1!", ErrPasswordTooShort},
		{"legacy default", "123456", ErrWeakPassword},
		{"admin", "ADMIN", ErrWeakPassword},
		{"too long", strings.Repeat("Aa1!", 20), ErrPasswordTooLong},
		{"valid", "Crony-Agent-2026", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ValidatePasswordStrength(tc.password)
			if err != tc.wantErr {
				t.Fatalf("ValidatePasswordStrength(%q) = %v, want %v", tc.password, err, tc.wantErr)
			}
		})
	}
}

func TestHashPasswordRejectsWeakInput(t *testing.T) {
	if _, err := HashPassword("123456"); err == nil {
		t.Fatal("HashPassword accepted the historical default password")
	}
}

func TestIsLegacyHash(t *testing.T) {
	if !IsLegacyHash("e10adc3949ba59abbe56e057f20f883e") {
		t.Fatal("32-hex string must be recognised as a legacy MD5 hash")
	}
	hash, err := bcrypt.GenerateFromPassword([]byte("Str0ng-Passw0rd!"), bcrypt.MinCost)
	if err != nil {
		t.Fatalf("bcrypt error = %v", err)
	}
	if IsLegacyHash(string(hash)) {
		t.Fatal("bcrypt hash must not be treated as legacy MD5")
	}
}
