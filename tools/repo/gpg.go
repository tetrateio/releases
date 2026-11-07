package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// gpgSign signs a file with the key of the fingerprint. gpg picks the
// signing subkey, so the primary key does not need to be present.
func gpgSign(fingerprint, in, out string, clear bool) error {
	mode := "--detach-sign"
	if clear {
		mode = "--clearsign"
	}
	cmd := exec.Command("gpg", "--yes", "--armor", "--digest-algo", "SHA512",
		"--local-user", fingerprint, mode, "--output", out, in)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err := cmd.Run(); err != nil {
		hint := ""
		if os.Getenv("GPG_TTY") == "" {
			hint = ". gpg asks for the passphrase on the terminal in GPG_TTY: run export GPG_TTY=$(tty) first"
		}
		return fmt.Errorf("gpg %s %s: %v: %s%s", mode, in, err, strings.TrimSpace(stderr.String()), hint)
	}
	return nil
}

// gpgVerify checks a detached signature with only the public key file in
// the keyring, and requires the signer to be the fingerprint.
func gpgVerify(key, fingerprint, sig, data string) error {
	_, err := gpgWithKey(key, fingerprint, "--verify", sig, data)
	return err
}

// gpgVerifyClear checks a cleartext signature and returns the signed text.
func gpgVerifyClear(key, fingerprint, signed string) ([]byte, error) {
	return gpgWithKey(key, fingerprint, "--output", "-", "--decrypt", signed)
}

func gpgWithKey(key, fingerprint string, args ...string) ([]byte, error) {
	home, err := os.MkdirTemp("", "repo-gpg-")
	if err != nil {
		return nil, err
	}
	defer os.RemoveAll(home)
	run := func(stdout *bytes.Buffer, status *os.File, args ...string) error {
		base := []string{"--homedir", home, "--batch", "--no-tty", "--quiet"}
		if status != nil {
			base = append(base, "--status-fd", "3")
		}
		cmd := exec.Command("gpg", append(base, args...)...)
		cmd.Stdout = stdout
		var stderr bytes.Buffer
		cmd.Stderr = &stderr
		if status != nil {
			cmd.ExtraFiles = []*os.File{status}
		}
		if err := cmd.Run(); err != nil {
			return fmt.Errorf("gpg %s: %v: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
		}
		return nil
	}
	if err := run(&bytes.Buffer{}, nil, "--import", key); err != nil {
		return nil, err
	}
	r, w, err := os.Pipe()
	if err != nil {
		return nil, err
	}
	statusc := make(chan []byte, 1)
	go func() {
		var b bytes.Buffer
		_, _ = b.ReadFrom(r)
		_ = r.Close()
		statusc <- b.Bytes()
	}()
	var out bytes.Buffer
	err = run(&out, w, args...)
	_ = w.Close()
	status := <-statusc
	if err != nil {
		return nil, err
	}
	if err := checkSigner(status, fingerprint); err != nil {
		return nil, err
	}
	return out.Bytes(), nil
}

// checkSigner requires one good signature whose primary key is the
// fingerprint. VALIDSIG names the signing key first and the primary key
// last.
func checkSigner(status []byte, fingerprint string) error {
	var found []string
	for line := range strings.SplitSeq(string(status), "\n") {
		f := strings.Fields(line)
		if len(f) >= 3 && f[0] == "[GNUPG:]" && f[1] == "VALIDSIG" {
			found = append(found, f[len(f)-1])
		}
	}
	if len(found) != 1 {
		return fmt.Errorf("want one valid signature, got %d", len(found))
	}
	if !strings.EqualFold(found[0], fingerprint) {
		return errors.New("the signature is from the key " + found[0] + ", not " + fingerprint)
	}
	return nil
}
