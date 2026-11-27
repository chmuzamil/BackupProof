package crypto

import "testing"

// Key derivation settings come from the storage's key files: out-of-range
// ones are refused with an error instead of crashing or exhausting the server.
func TestKDFSettingsBounded(t *testing.T) {
	ok, err := DefaultKDF()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ok.Derive([]byte("password")); err != nil {
		t.Fatalf("the defaults: %v", err)
	}
	for name, p := range map[string]KDFParams{
		"zero rounds": {Algo: "argon2id", Time: 0, Memory: 64 * 1024, Threads: 4, Salt: ok.Salt},
		"many rounds": {Algo: "argon2id", Time: 1000, Memory: 64 * 1024, Threads: 4, Salt: ok.Salt},
		"no memory":   {Algo: "argon2id", Time: 3, Memory: 0, Threads: 4, Salt: ok.Salt},
		"huge memory": {Algo: "argon2id", Time: 3, Memory: 1 << 30, Threads: 4, Salt: ok.Salt},
		"no threads":  {Algo: "argon2id", Time: 3, Memory: 64 * 1024, Threads: 0, Salt: ok.Salt},
		"no salt":     {Algo: "argon2id", Time: 3, Memory: 64 * 1024, Threads: 4},
		"other kdf":   {Algo: "scrypt", Time: 3, Memory: 64 * 1024, Threads: 4, Salt: ok.Salt},
	} {
		if _, err := p.Derive([]byte("password")); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}
