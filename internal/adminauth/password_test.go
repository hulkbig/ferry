package adminauth

import (
	"strings"
	"testing"
)

func TestPasswordArgon2idLifecycle(t *testing.T) {
	hash, err := HashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(hash, "$argon2id$v=19$m=65536,t=3,p=2$") {
		t.Fatalf("unexpected hash parameters: %s", hash)
	}
	if ok, err := VerifyPassword(hash, "correct horse battery staple"); err != nil || !ok {
		t.Fatalf("valid password: ok=%v err=%v", ok, err)
	}
	if ok, err := VerifyPassword(hash, "incorrect password"); err != nil || ok {
		t.Fatalf("invalid password: ok=%v err=%v", ok, err)
	}
	if _, err := HashPassword("too-short"); err == nil {
		t.Fatal("short password accepted")
	}
}
