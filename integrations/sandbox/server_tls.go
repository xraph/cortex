package sandboxbroker

import (
	"bytes"
	"crypto"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"math/big"
	"time"
)

func freezeCertificate(input tls.Certificate) (tls.Certificate, error) {
	if len(input.Certificate) == 0 || len(input.Certificate) > 8 {
		return tls.Certificate{}, fmt.Errorf("cortex: invalid sandbox TLS certificate chain")
	}
	output := tls.Certificate{Certificate: make([][]byte, len(input.Certificate))}
	total := 0
	var leaf *x509.Certificate
	for i, der := range input.Certificate {
		total += len(der)
		if len(der) == 0 || total > 128<<10 {
			return tls.Certificate{}, fmt.Errorf("cortex: oversized sandbox TLS certificate chain")
		}
		output.Certificate[i] = bytes.Clone(der)
		parsed, err := x509.ParseCertificate(output.Certificate[i])
		if err != nil {
			return tls.Certificate{}, fmt.Errorf("cortex: invalid sandbox TLS certificate chain: %w", err)
		}
		if i == 0 {
			leaf = parsed
		}
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || !now.Before(leaf.NotAfter) {
		return tls.Certificate{}, fmt.Errorf("cortex: sandbox TLS certificate is not current")
	}
	key, err := encodeTLSKey(input.PrivateKey)
	if err != nil {
		return tls.Certificate{}, fmt.Errorf("cortex: unsupported sandbox TLS private key: %w", err)
	}
	defer clear(key)
	if len(key) > 16<<10 {
		return tls.Certificate{}, fmt.Errorf("cortex: oversized sandbox TLS private key")
	}
	output.PrivateKey, err = x509.ParsePKCS8PrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	signer, ok := output.PrivateKey.(crypto.Signer)
	if !ok {
		return tls.Certificate{}, fmt.Errorf("cortex: sandbox TLS key cannot sign")
	}
	public, err := x509.MarshalPKIXPublicKey(signer.Public())
	if err != nil {
		return tls.Certificate{}, err
	}
	expected, err := x509.MarshalPKIXPublicKey(leaf.PublicKey)
	if err != nil || !bytes.Equal(public, expected) {
		return tls.Certificate{}, fmt.Errorf("cortex: sandbox TLS key does not match certificate")
	}
	// The leaf is parsed from the copied DER. Caller-supplied parsed state is ignored.
	output.Leaf = leaf
	if len(input.OCSPStaple) > 64<<10 || len(input.SignedCertificateTimestamps) > 16 {
		return tls.Certificate{}, fmt.Errorf("cortex: oversized sandbox TLS certificate metadata")
	}
	output.OCSPStaple = bytes.Clone(input.OCSPStaple)
	output.SignedCertificateTimestamps = make([][]byte, len(input.SignedCertificateTimestamps))
	total = 0
	for i, timestamp := range input.SignedCertificateTimestamps {
		total += len(timestamp)
		if total > 64<<10 {
			return tls.Certificate{}, fmt.Errorf("cortex: oversized sandbox TLS timestamps")
		}
		output.SignedCertificateTimestamps[i] = bytes.Clone(timestamp)
	}
	return output, nil
}

// encodeTLSKey bounds host key material before invoking encoders that assume
// well-formed keys. RSA precomputation is performed only on a detached key.
func encodeTLSKey(input any) ([]byte, error) {
	invalid := fmt.Errorf("cortex: invalid sandbox TLS software key")
	switch key := input.(type) {
	case *ecdsa.PrivateKey:
		if key == nil || key.X == nil || key.Y == nil {
			return nil, invalid
		}
		//nolint:staticcheck // Bytes dereferences the deprecated scalar, so malformed host keys need this nil guard.
		if key.D == nil {
			return nil, invalid
		}
		if key.Curve != elliptic.P256() && key.Curve != elliptic.P384() && key.Curve != elliptic.P521() {
			return nil, invalid
		}
		raw, err := key.Bytes()
		if err != nil {
			return nil, err
		}
		defer clear(raw)
		detached, err := ecdsa.ParseRawPrivateKey(key.Curve, raw)
		if err != nil {
			return nil, err
		}
		input = detached
	case ed25519.PrivateKey:
		if len(key) != ed25519.PrivateKeySize {
			return nil, invalid
		}
	case *rsa.PrivateKey:
		if key == nil || key.N == nil || key.D == nil || key.N.BitLen() < 2048 || key.N.BitLen() > 8192 || key.D.Sign() <= 0 || key.D.BitLen() > key.N.BitLen() || len(key.Primes) != 2 {
			return nil, invalid
		}
		detached := &rsa.PrivateKey{PublicKey: rsa.PublicKey{N: new(big.Int).Set(key.N), E: key.E}, D: new(big.Int).Set(key.D), Primes: make([]*big.Int, 2)}
		for i, prime := range key.Primes {
			if prime == nil || prime.Sign() <= 0 || prime.BitLen() > key.N.BitLen() {
				return nil, invalid
			}
			detached.Primes[i] = new(big.Int).Set(prime)
		}
		input = detached
	default:
		return nil, invalid
	}
	return x509.MarshalPKCS8PrivateKey(input)
}
