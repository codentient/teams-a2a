package teams

import (
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"math/big"
	"testing"
	"time"
)

// Security review L3: both JWK forms get the same RSA checks.
func TestSigningKeyChecksAreTheSameForBothForms(t *testing.T) {
	big2048, _ := rsa.GenerateKey(rand.Reader, 2048)
	small, _ := rsa.GenerateKey(rand.Reader, 1024)
	ne := func(pub *rsa.PublicKey, e int) jwk {
		return jwk{Kid: "k", N: base64.RawURLEncoding.EncodeToString(pub.N.Bytes()), E: base64.RawURLEncoding.EncodeToString(big.NewInt(int64(e)).Bytes())}
	}
	x5c := func(priv *rsa.PrivateKey) jwk {
		tmpl := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "x"}, NotAfter: time.Now().Add(time.Hour)}
		der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &priv.PublicKey, priv)
		if err != nil {
			t.Fatal(err)
		}
		return jwk{Kid: "k", X5c: []string{base64.StdEncoding.EncodeToString(der)}}
	}
	for name, tc := range map[string]struct {
		k  jwk
		ok bool
	}{
		"n/e 2048, e=65537": {ne(&big2048.PublicKey, 65537), true},
		"x5c 2048":          {x5c(big2048), true},
		"n/e 1024":          {ne(&small.PublicKey, 65537), false},
		"x5c 1024":          {x5c(small), false},
		"n/e e=1":           {ne(&big2048.PublicKey, 1), false},
		"n/e even e":        {ne(&big2048.PublicKey, 65536), false},
		"n/e e=2":           {ne(&big2048.PublicKey, 2), false},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := rsaKey(tc.k)
			if (err == nil) != tc.ok {
				t.Fatalf("ok=%v, err=%v", tc.ok, err)
			}
		})
	}
}
