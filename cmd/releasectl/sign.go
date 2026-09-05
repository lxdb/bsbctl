package main

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"slices"
	"time"

	"github.com/lxdb/bsbctl/internal/catalog"
	"github.com/lxdb/bsbctl/internal/releasekeys"
)

const maxEncodedSigningKeyBytes = 256

var signingKeyIDPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,63}$`)

var (
	catalogKeyringLoader     = releasekeys.CatalogKeyring
	catalogVerificationClock = time.Now
)

type catalogSignatureEnvelope struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Signature string `json:"signature"`
}

func runSignCatalog(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	flags := newFlagSet("sign-catalog")
	catalogPath := flags.String("catalog", "", "catalog JSON file")
	keyID := flags.String("key-id", "", "authorized public-key identifier")
	outputPath := flags.String("out", "", "signature envelope output")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *catalogPath == "" || !signingKeyIDPattern.MatchString(*keyID) || *outputPath == "" {
		_, _ = fmt.Fprintln(stderr, "catalog signing failed: invalid arguments")
		return exitFailure
	}
	catalogData, err := readBoundedFile(*catalogPath)
	if err != nil || !json.Valid(catalogData) {
		_, _ = fmt.Fprintln(stderr, "catalog signing failed: invalid catalog")
		return exitFailure
	}
	private, err := readSigningKey(stdin)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "catalog signing failed: invalid signing input")
		return exitFailure
	}
	defer clear(private)
	keyring, err := catalogKeyringLoader()
	trackedPublic, authorized := keyring[*keyID]
	derivedPublic := private.Public().(ed25519.PublicKey)
	if err != nil || !authorized || !bytes.Equal(derivedPublic, trackedPublic) {
		clear(derivedPublic)
		_, _ = fmt.Fprintln(stderr, "catalog signing failed: signing key is not authorized")
		return exitFailure
	}
	clear(derivedPublic)
	signature := ed25519.Sign(private, catalogData)
	envelope, err := json.Marshal(catalogSignatureEnvelope{
		KeyID: *keyID, Algorithm: "ed25519", Signature: base64.StdEncoding.EncodeToString(signature),
	})
	clear(signature)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "catalog signing failed: encode envelope")
		return exitFailure
	}
	envelope = append(envelope, '\n')
	if err := writeExclusiveFile(*outputPath, envelope); err != nil {
		_, _ = fmt.Fprintln(stderr, "catalog signing failed: write envelope")
		return exitFailure
	}
	_, _ = fmt.Fprintln(stdout, "catalog signature: written")
	return exitSuccess
}

func runVerifyCatalog(args []string, stdout, stderr io.Writer) int {
	flags := newFlagSet("verify-catalog")
	catalogPath := flags.String("catalog", "", "catalog JSON file")
	signaturePath := flags.String("signature", "", "signature envelope file")
	if err := flags.Parse(args); err != nil || flags.NArg() != 0 || *catalogPath == "" || *signaturePath == "" {
		_, _ = fmt.Fprintln(stderr, "catalog verification failed")
		return exitFailure
	}
	catalogData, catalogErr := readBoundedFile(*catalogPath)
	signatureData, signatureErr := readBoundedFile(*signaturePath)
	keyring, keyringErr := catalogKeyringLoader()
	if catalogErr != nil || signatureErr != nil || keyringErr != nil {
		_, _ = fmt.Fprintln(stderr, "catalog verification failed")
		return exitFailure
	}
	verified, err := catalog.Verify(catalogData, signatureData, keyring, 0, catalogVerificationClock().UTC())
	if err != nil {
		_, _ = fmt.Fprintln(stderr, "catalog verification failed")
		return exitFailure
	}
	_, _ = fmt.Fprintf(stdout, "stable catalog: verified sequence %d\n", verified.Sequence)
	return exitSuccess
}

func readSigningKey(input io.Reader) (ed25519.PrivateKey, error) {
	data, err := io.ReadAll(io.LimitReader(input, maxEncodedSigningKeyBytes+1))
	defer clear(data)
	if err != nil || len(data) == 0 || len(data) > maxEncodedSigningKeyBytes || !bytes.Equal(bytes.TrimSpace(data), data) {
		return nil, errors.New("signing key input is invalid")
	}
	decoded := make([]byte, base64.StdEncoding.DecodedLen(len(data)))
	defer clear(decoded)
	decodedBytes, err := base64.StdEncoding.Strict().Decode(decoded, data)
	if err != nil || decodedBytes != ed25519.PrivateKeySize {
		return nil, errors.New("signing key input is invalid")
	}
	decoded = decoded[:decodedBytes]
	canonical := make([]byte, base64.StdEncoding.EncodedLen(len(decoded)))
	defer clear(canonical)
	base64.StdEncoding.Encode(canonical, decoded)
	if !bytes.Equal(canonical, data) {
		return nil, errors.New("signing key input is invalid")
	}
	private := ed25519.PrivateKey(slices.Clone(decoded))
	return private, nil
}

func writeExclusiveFile(path string, data []byte) error {
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o644)
	if err != nil {
		return err
	}
	writeErr := writeAll(file, data)
	closeErr := file.Close()
	if writeErr != nil || closeErr != nil {
		return errors.Join(writeErr, closeErr)
	}
	return nil
}

func writeAll(output io.Writer, data []byte) error {
	for len(data) > 0 {
		written, err := output.Write(data)
		if err != nil {
			return err
		}
		if written < 1 || written > len(data) {
			return io.ErrShortWrite
		}
		data = data[written:]
	}
	return nil
}
