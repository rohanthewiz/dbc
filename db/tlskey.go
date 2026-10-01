package db

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/pbkdf2"
	"crypto/sha1"
	"crypto/sha256"
	"crypto/sha512"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/asn1"
	"encoding/pem"
	"errors"
	"fmt"
	"hash"
	"os"
	"strings"

	"github.com/rohanthewiz/serr"

	"github.com/rohanthewiz/dbc/config"
)

// A passphrase-protected client key (tls_key + tls_key_password).
//
// WHY dbc DECRYPTS THE KEY ITSELF. Neither driver can do it for both
// encodings a key comes in:
//
//   - pgx's sslpassword handles only legacy PEM encryption (a
//     "Proc-Type: 4,ENCRYPTED" header, as `openssl rsa -aes256 -traditional`
//     writes). It fails on PKCS#8 "ENCRYPTED PRIVATE KEY", which is what
//     OpenSSL 3 writes by default (`genpkey -aes256`, `pkcs8 -topk8`).
//   - go-sql-driver/mysql takes only a *tls.Config, so the key must arrive
//     decrypted whatever its encoding.
//
// So the key is decrypted here, once per pool open, into a tls.Certificate,
// which both routes then use:
//
//	clientCert(t)
//	  ├─ no tls_key_password ─► tls.LoadX509KeyPair, as before
//	  └─ tls_key_password    ─► os.LookupEnv(var) ─► decryptKeyPEM ─► tls.X509KeyPair
//	                                                   ├─ ENCRYPTED PRIVATE KEY ─► PKCS#8 PBES2 (below)
//	                                                   └─ Proc-Type ENCRYPTED   ─► x509.DecryptPEMBlock
//
//	mysql ─► mysqlTLS puts it in tls.Config.Certificates
//	pgx   ─► pgTLSDSN leaves sslcert/sslkey out of the DSN; openPool puts it
//	         in the parsed config's TLSConfig (and every TLS fallback's)
//
// PKCS#8 PBES2 is decoded with encoding/asn1 and the standard library's
// PBKDF2 and AES rather than a third-party package: it is one well-pinned
// structure (RFC 8018), and this is the only place dbc needs it. Supported
// are what OpenSSL writes for -v2 aes-*-cbc: PBKDF2 with an HMAC-SHA1/224/
// 256/384/512 PRF, and AES-128/192/256-CBC. scrypt, DES-EDE3 and the older
// PBES1 schemes are refused with a message saying how to re-encrypt.

// pbes2 and friends are the object identifiers an EncryptedPrivateKeyInfo
// names its algorithms by (RFC 8018 appendix C, NIST for AES, RFC 7914 for
// scrypt).
var (
	oidPBES2  = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 13}
	oidPBKDF2 = asn1.ObjectIdentifier{1, 2, 840, 113549, 1, 5, 12}
	oidScrypt = asn1.ObjectIdentifier{1, 3, 6, 1, 4, 1, 11591, 4, 11}

	oidHMACSHA1   = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 7}
	oidHMACSHA224 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 8}
	oidHMACSHA256 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 9}
	oidHMACSHA384 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 10}
	oidHMACSHA512 = asn1.ObjectIdentifier{1, 2, 840, 113549, 2, 11}

	oidAES128CBC = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 2}
	oidAES192CBC = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 22}
	oidAES256CBC = asn1.ObjectIdentifier{2, 16, 840, 1, 101, 3, 4, 1, 42}
)

// errWrongPassphrase is what a decryption that produced garbage reports.
// The two causes cannot be told apart from the bytes alone: a wrong
// passphrase decrypts to noise, and so does a key that was never encrypted
// the way its header claims.
var errWrongPassphrase = errors.New("wrong tls_key_password, or not an encrypted key")

// encryptedPrivateKeyInfo is PKCS#8's EncryptedPrivateKeyInfo (RFC 5958):
// the scheme, then the encrypted PrivateKeyInfo.
type encryptedPrivateKeyInfo struct {
	Algo          pkix.AlgorithmIdentifier
	EncryptedData []byte
}

// pbes2Params is RFC 8018's PBES2-params: how the key is derived from the
// passphrase, and what cipher it feeds.
type pbes2Params struct {
	KeyDerivationFunc pkix.AlgorithmIdentifier
	EncryptionScheme  pkix.AlgorithmIdentifier
}

// pbkdf2Params is RFC 8018's PBKDF2-params. The salt is a CHOICE whose
// only defined arm is an OCTET STRING. KeyLength is optional (the cipher
// implies it), and so is the PRF, which defaults to HMAC-SHA1 — OpenSSL
// leaves it out exactly when it is SHA1.
type pbkdf2Params struct {
	Salt      []byte
	Iter      int
	KeyLength int                      `asn1:"optional"`
	PRF       pkix.AlgorithmIdentifier `asn1:"optional"`
}

// clientCert loads t's client certificate and key, decrypting the key when
// t has a tls_key_password. The caller has checked t.TLSCert is set.
//
// The passphrase is read from the environment here, at the open, and
// nowhere else: an unset variable is an error naming it, never an empty
// passphrase tried against the key. Errors name the files but never the
// passphrase.
func clientCert(t config.TLSOpts) (tls.Certificate, error) {
	if t.TLSKeyPassword == "" {
		pair, err := tls.LoadX509KeyPair(t.TLSCert, t.TLSKey)
		if err != nil {
			// the key names and paths go in the message: it is shown as is
			// (serr's fields are not), and which file is wrong is the point
			return tls.Certificate{}, serr.Wrap(fmt.Errorf("tls_cert %s / tls_key %s: %w", t.TLSCert, t.TLSKey, err))
		}
		return pair, nil
	}
	name := t.KeyPasswordVar()
	if name == "" {
		// config.TLSOpts.Check refuses this first; failing closed covers a
		// caller that skipped it — and the value is not echoed
		return tls.Certificate{}, serr.New("tls_key_password must name an environment variable, as ${VAR}")
	}
	pass, ok := os.LookupEnv(name)
	if !ok {
		return tls.Certificate{}, serr.New("tls_key_password: $" + name + " is not set — export the key's passphrase in it")
	}
	keyPEM, err := os.ReadFile(t.TLSKey)
	if err != nil {
		return tls.Certificate{}, serr.Wrap(fmt.Errorf("tls_key: %w", err)) // os's error names the path
	}
	plain, err := decryptKeyPEM(keyPEM, []byte(pass))
	if err != nil {
		return tls.Certificate{}, serr.Wrap(fmt.Errorf("tls_key %s: %w", t.TLSKey, err))
	}
	certPEM, err := os.ReadFile(t.TLSCert)
	if err != nil {
		return tls.Certificate{}, serr.Wrap(fmt.Errorf("tls_cert: %w", err))
	}
	pair, err := tls.X509KeyPair(certPEM, plain)
	if err != nil {
		return tls.Certificate{}, serr.Wrap(fmt.Errorf("tls_cert %s / tls_key %s: %w", t.TLSCert, t.TLSKey, err))
	}
	return pair, nil
}

// decryptKeyPEM finds the private key block in keyPEM, decrypts it with
// pass, and returns it as unencrypted PEM for tls.X509KeyPair.
//
// Blocks before the key are skipped: `openssl ecparam -genkey` writes an
// "EC PARAMETERS" block first. A key that is not encrypted is refused
// rather than used: a tls_key_password beside it means the user expects
// encryption, and a key quietly stored in the clear is worth saying so.
func decryptKeyPEM(keyPEM, pass []byte) ([]byte, error) {
	var block *pem.Block
	for rest := keyPEM; ; {
		block, rest = pem.Decode(rest)
		if block == nil {
			return nil, errors.New("no PEM private key in the file")
		}
		if strings.HasSuffix(block.Type, "PRIVATE KEY") {
			break
		}
	}
	switch {
	case block.Type == "ENCRYPTED PRIVATE KEY":
		der, err := decryptPKCS8(block.Bytes, pass)
		if err != nil {
			return nil, err
		}
		return pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: der}), nil
	// x509.IsEncryptedPEMBlock and DecryptPEMBlock are deprecated because
	// legacy PEM encryption is weak (an MD5 key derivation, no integrity
	// check), hence the nolint. They are used only to READ a key the user
	// already has in that format, as pgx's sslpassword does; nothing here
	// writes one.
	case x509.IsEncryptedPEMBlock(block): //nolint:staticcheck // reading a legacy key, see above
		der, err := x509.DecryptPEMBlock(block, pass) //nolint:staticcheck // reading a legacy key, see above
		if err != nil {
			return nil, errWrongPassphrase
		}
		// The padding check DecryptPEMBlock makes passes by chance for
		// about one wrong passphrase in 256; parsing the result is the
		// real test.
		if !parsesAsKey(block.Type, der) {
			return nil, errWrongPassphrase
		}
		return pem.EncodeToMemory(&pem.Block{Type: block.Type, Bytes: der}), nil
	}
	return nil, errors.New("the key is not encrypted — remove tls_key_password, or encrypt the key")
}

// parsesAsKey reports whether der is a private key of the kind a legacy
// PEM block of type typ holds.
func parsesAsKey(typ string, der []byte) bool {
	var err error
	switch typ {
	case "RSA PRIVATE KEY":
		_, err = x509.ParsePKCS1PrivateKey(der)
	case "EC PRIVATE KEY":
		_, err = x509.ParseECPrivateKey(der)
	default:
		_, err = x509.ParsePKCS8PrivateKey(der)
	}
	return err == nil
}

// decryptPKCS8 decrypts a DER EncryptedPrivateKeyInfo with pass, returning
// the DER PrivateKeyInfo inside.
//
//	EncryptedPrivateKeyInfo
//	  ├─ algorithm: PBES2
//	  │    ├─ keyDerivationFunc: PBKDF2 (salt, iterations, [keyLength], [prf])
//	  │    └─ encryptionScheme:  AES-n-CBC (IV)
//	  └─ encryptedData
//
//	key = PBKDF2(prf, pass, salt, iterations, n/8)
//	PrivateKeyInfo = unpad(AES-CBC-decrypt(key, IV, encryptedData))
func decryptPKCS8(der, pass []byte) ([]byte, error) {
	var info encryptedPrivateKeyInfo
	if rest, err := asn1.Unmarshal(der, &info); err != nil || len(rest) != 0 {
		return nil, errors.New("not a PKCS#8 encrypted private key")
	}
	if !info.Algo.Algorithm.Equal(oidPBES2) {
		return nil, unsupportedScheme("encryption scheme " + info.Algo.Algorithm.String() + " (PBES1 or PKCS#12)")
	}
	var params pbes2Params
	if _, err := asn1.Unmarshal(info.Algo.Parameters.FullBytes, &params); err != nil {
		return nil, errors.New("bad PBES2 parameters in the key")
	}

	// the cipher first, since it fixes the derived key's length
	var keyLen int
	switch alg := params.EncryptionScheme.Algorithm; {
	case alg.Equal(oidAES128CBC):
		keyLen = 16
	case alg.Equal(oidAES192CBC):
		keyLen = 24
	case alg.Equal(oidAES256CBC):
		keyLen = 32
	default:
		return nil, unsupportedScheme("cipher " + alg.String() + " (only AES-CBC is)")
	}
	var iv []byte
	if _, err := asn1.Unmarshal(params.EncryptionScheme.Parameters.FullBytes, &iv); err != nil || len(iv) != aes.BlockSize {
		return nil, errors.New("bad AES-CBC IV in the key")
	}

	switch kdf := params.KeyDerivationFunc.Algorithm; {
	case kdf.Equal(oidScrypt):
		return nil, unsupportedScheme("key derivation scrypt")
	case !kdf.Equal(oidPBKDF2):
		return nil, unsupportedScheme("key derivation " + kdf.String())
	}
	var kp pbkdf2Params
	if _, err := asn1.Unmarshal(params.KeyDerivationFunc.Parameters.FullBytes, &kp); err != nil {
		return nil, errors.New("bad PBKDF2 parameters in the key")
	}
	if kp.KeyLength != 0 && kp.KeyLength != keyLen {
		return nil, fmt.Errorf("the key's PBKDF2 length %d does not fit its cipher", kp.KeyLength)
	}
	prf, err := pbkdf2PRF(kp.PRF.Algorithm)
	if err != nil {
		return nil, err
	}
	key, err := pbkdf2.Key(prf, string(pass), kp.Salt, kp.Iter, keyLen)
	if err != nil {
		return nil, fmt.Errorf("derive the key: %w", err)
	}

	data := info.EncryptedData
	if len(data) == 0 || len(data)%aes.BlockSize != 0 {
		return nil, errors.New("the encrypted key is not a whole number of AES blocks")
	}
	blk, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	plain := make([]byte, len(data))
	cipher.NewCBCDecrypter(blk, iv).CryptBlocks(plain, data)

	// PKCS#7 padding: n bytes of value n, 1 ≤ n ≤ 16. A wrong passphrase
	// almost always breaks it; the parse below catches the rest.
	n := int(plain[len(plain)-1])
	if n == 0 || n > aes.BlockSize {
		return nil, errWrongPassphrase
	}
	for _, b := range plain[len(plain)-n:] {
		if int(b) != n {
			return nil, errWrongPassphrase
		}
	}
	plain = plain[:len(plain)-n]
	if _, err = x509.ParsePKCS8PrivateKey(plain); err != nil {
		return nil, errWrongPassphrase
	}
	return plain, nil
}

// pbkdf2PRF maps a PBKDF2 PRF identifier to its hash. An absent PRF (the
// zero OID) is the default, HMAC-SHA1.
func pbkdf2PRF(oid asn1.ObjectIdentifier) (func() hash.Hash, error) {
	switch {
	case len(oid) == 0, oid.Equal(oidHMACSHA1):
		return sha1.New, nil
	case oid.Equal(oidHMACSHA224):
		return sha256.New224, nil
	case oid.Equal(oidHMACSHA256):
		return sha256.New, nil
	case oid.Equal(oidHMACSHA384):
		return sha512.New384, nil
	case oid.Equal(oidHMACSHA512):
		return sha512.New, nil
	}
	return nil, unsupportedScheme("PBKDF2 PRF " + oid.String())
}

// unsupportedScheme says what is not supported, and how to get a key that
// is: OpenSSL can re-encrypt any key it can read.
func unsupportedScheme(what string) error {
	return errors.New("the key uses " + what + ", which dbc cannot decrypt — re-encrypt it with " +
		"`openssl pkcs8 -topk8 -v2 aes-256-cbc -v2prf hmacWithSHA256 -in old.key -out new.key`")
}
