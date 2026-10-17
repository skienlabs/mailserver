package core

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/x509"
	"encoding/base64"
	"encoding/hex"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"runtime"
	"strings"

	"golang.org/x/crypto/argon2"
)

// Argon2id parameters (OWASP recommended minimum: m=19MiB, t=2, p=1).
const (
	argonTime    = 2
	argonMemory  = 19 * 1024
	argonThreads = 1
	argonKeyLen  = 32
)

// hashSlots caps concurrent Argon2 computations (19 MiB each) so a flood of logins
// can't exhaust memory or starve other work.
var hashSlots = make(chan struct{}, max(2, runtime.NumCPU()))

func argonKey(pw, salt []byte, t, m uint32, p uint8, n uint32) []byte {
	hashSlots <- struct{}{}
	defer func() { <-hashSlots }()
	return argon2.IDKey(pw, salt, t, m, p, n)
}

// HashPassword returns an encoded argon2id hash.
func HashPassword(pw string) (string, error) {
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return "", err
	}
	key := argonKey([]byte(pw), salt, argonTime, argonMemory, argonThreads, argonKeyLen)
	return fmt.Sprintf("$argon2id$v=%d$m=%d,t=%d,p=%d$%s$%s", argon2.Version, argonMemory, argonTime, argonThreads,
		base64.RawStdEncoding.EncodeToString(salt), base64.RawStdEncoding.EncodeToString(key)), nil
}

// CheckPassword verifies pw against an encoded argon2id hash in constant time.
func CheckPassword(encoded, pw string) bool {
	parts := strings.Split(encoded, "$")
	if len(parts) != 6 || parts[1] != "argon2id" {
		return false
	}
	var m, t uint32
	var p uint8
	if _, err := fmt.Sscanf(parts[3], "m=%d,t=%d,p=%d", &m, &t, &p); err != nil {
		return false
	}
	salt, err := base64.RawStdEncoding.DecodeString(parts[4])
	if err != nil {
		return false
	}
	want, err := base64.RawStdEncoding.DecodeString(parts[5])
	if err != nil || len(want) == 0 {
		return false
	}
	if m > 256*1024 || t > 10 || p == 0 || p > 16 || len(want) > 64 {
		return false // refuse absurd parameters from a corrupted hash
	}
	got := argonKey([]byte(pw), salt, t, m, p, uint32(len(want)))
	return subtle.ConstantTimeCompare(got, want) == 1
}

// dummyHash is used to keep login timing constant for unknown users.
var dummyHash, _ = HashPassword("skienmail-timing-equaliser")

// RandomToken returns n random bytes, URL-safe base64 encoded.
func RandomToken(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return base64.RawURLEncoding.EncodeToString(b)
}

// RandomHex returns n random bytes hex encoded.
func RandomHex(n int) string {
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		panic(err)
	}
	return hex.EncodeToString(b)
}

func sha256Hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// GeneratePassword creates a readable random password like "Kiva-7Rp2-Tumo-x9Qa".
func GeneratePassword() string {
	const consonants = "bcdfghjkmnpqrstvwxz"
	const vowels = "aeiou"
	const alnum = "abcdefghjkmnpqrstuvwxyzABCDEFGHJKLMNPQRSTUVWXYZ23456789"
	pick := func(set string) byte {
		n, _ := rand.Int(rand.Reader, big.NewInt(int64(len(set))))
		return set[n.Int64()]
	}
	word := func() string {
		b := []byte{pick(consonants), pick(vowels), pick(consonants), pick(vowels)}
		b[0] = b[0] - 'a' + 'A'
		return string(b)
	}
	rnd := func() string {
		return string([]byte{pick(alnum), pick(alnum), pick(alnum), pick(alnum)})
	}
	return word() + "-" + rnd() + "-" + word() + "-" + rnd()
}

// ValidatePassword enforces a minimal password policy.
func ValidatePassword(pw string) error {
	if len(pw) < 8 {
		return errors.New("Password must be at least 8 characters")
	}
	if len(pw) > 256 {
		return errors.New("Password is too long")
	}
	return nil
}

// GenerateDKIMKey creates a 2048-bit RSA key in PKCS#8 PEM form.
func GenerateDKIMKey() (string, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		return "", err
	}
	return string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der})), nil
}

// ParseDKIMKey parses a PEM private key (PKCS#8 or PKCS#1).
func ParseDKIMKey(pemStr string) (*rsa.PrivateKey, error) {
	block, _ := pem.Decode([]byte(pemStr))
	if block == nil {
		return nil, errors.New("invalid DKIM key PEM")
	}
	if k, err := x509.ParsePKCS8PrivateKey(block.Bytes); err == nil {
		if rk, ok := k.(*rsa.PrivateKey); ok {
			return rk, nil
		}
		return nil, errors.New("DKIM key is not RSA")
	}
	return x509.ParsePKCS1PrivateKey(block.Bytes)
}

// DKIMRecord returns the TXT record value for the public half of the key.
func DKIMRecord(pemStr string) (string, error) {
	k, err := ParseDKIMKey(pemStr)
	if err != nil {
		return "", err
	}
	der, err := x509.MarshalPKIXPublicKey(&k.PublicKey)
	if err != nil {
		return "", err
	}
	return "v=DKIM1; k=rsa; p=" + base64.StdEncoding.EncodeToString(der), nil
}
