# Encrypted client-key fixtures

Throwaway keys and self-signed certificates for `db/tlskey_test.go`. They
protect nothing. Every encrypted key's passphrase is `dbc-test-pass`.

| File | What it is |
| --- | --- |
| `rsa.key`, `ec.key` | the unencrypted keys (PKCS#8) |
| `rsa.crt`, `ec.crt` | a self-signed certificate for each, valid for 100 years |
| `rsa-legacy.key`, `ec-legacy.key` | legacy PEM encryption (`Proc-Type: 4,ENCRYPTED`, AES-256-CBC) |
| `rsa-pkcs8.key`, `ec-pkcs8.key` | PKCS#8 PBES2: PBKDF2-HMAC-SHA256 + AES-256-CBC |
| `ec-pkcs8-aes128-sha1.key` | PKCS#8 PBES2: PBKDF2-HMAC-SHA1 (the default PRF, so omitted) + AES-128-CBC |
| `ec-scrypt.key` | PKCS#8 PBES2 with scrypt — not supported, must be refused |
| `ec-des3.key` | PKCS#8 PBES2 with DES-EDE3-CBC — not supported, must be refused |

Made with OpenSSL 3.6:

```sh
P=pass:dbc-test-pass
openssl genpkey -algorithm RSA -pkeyopt rsa_keygen_bits:2048 -out rsa.key
openssl req -x509 -new -key rsa.key -subj /CN=dbc-rsa-client -days 36500 -out rsa.crt
openssl genpkey -algorithm EC -pkeyopt ec_paramgen_curve:P-256 -out ec.key
openssl req -x509 -new -key ec.key -subj /CN=dbc-ec-client -days 36500 -out ec.crt
openssl rsa -in rsa.key -aes256 -traditional -passout $P -out rsa-legacy.key
openssl ec -in ec.key -aes256 -passout $P -out ec-legacy.key
openssl pkcs8 -topk8 -in rsa.key -v2 aes-256-cbc -v2prf hmacWithSHA256 -passout $P -out rsa-pkcs8.key
openssl pkcs8 -topk8 -in ec.key -v2 aes-256-cbc -v2prf hmacWithSHA256 -passout $P -out ec-pkcs8.key
openssl pkcs8 -topk8 -in ec.key -v2 aes-128-cbc -v2prf hmacWithSHA1 -passout $P -out ec-pkcs8-aes128-sha1.key
openssl pkcs8 -topk8 -in ec.key -scrypt -passout $P -out ec-scrypt.key
openssl pkcs8 -topk8 -in ec.key -v2 des3 -passout $P -out ec-des3.key
```
