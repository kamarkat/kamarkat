package main

import (
	"strings"
	"testing"
)

// A malformed credential enrols silently and then fails as "the bucket is
// unreachable" -- during a recovery, which is the worst moment to be debugging INI
// formatting. So the shape is checked before anything is escrowed.
func TestValidateStoreCredentialChecksTheShapeThePluginReads(t *testing.T) {
	good := "[default]\naws_access_key_id=AKIAEXAMPLE\naws_secret_access_key=supersecret\n"
	if err := validateStoreCredential(good); err != nil {
		t.Fatalf("a well-formed credential was refused: %v", err)
	}

	// Comments, blank lines, spacing around the separator and a non-default profile
	// name are all ordinary INI and must not be rejected.
	tolerated := "# hetzner\n\n[backup]\n  aws_access_key_id = AKIA2\n" +
		"; a semicolon comment\naws_secret_access_key = s3cret\n"
	if err := validateStoreCredential(tolerated); err != nil {
		t.Errorf("ordinary INI was refused: %v", err)
	}

	for name, in := range map[string]string{
		"no profile section":   "aws_access_key_id=A\naws_secret_access_key=B\n",
		"empty profile name":   "[]\naws_access_key_id=A\naws_secret_access_key=B\n",
		"no key id":            "[default]\naws_secret_access_key=B\n",
		"no secret":            "[default]\naws_access_key_id=A\n",
		"empty key id":         "[default]\naws_access_key_id=\naws_secret_access_key=B\n",
		"empty secret":         "[default]\naws_access_key_id=A\naws_secret_access_key=\n",
		"only comments":        "# nothing here\n; nor here\n",
		"json instead of ini":  `{"aws_access_key_id":"A","aws_secret_access_key":"B"}`,
		"bare key=value pairs": "key=A\nsecret=B\n",
	} {
		if err := validateStoreCredential(in); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
}

// Errors name what is missing and never what was supplied: an error that quotes the
// credential puts it in a terminal and a log.
func TestStoreCredentialErrorsNeverCarryTheValue(t *testing.T) {
	const secretValue = "THIS-IS-THE-SECRET-VALUE"
	err := validateStoreCredential("[default]\naws_access_key_id=" + secretValue + "\n")
	if err == nil {
		t.Fatal("a credential with no secret access key was accepted")
	}
	if strings.Contains(err.Error(), secretValue) {
		t.Errorf("the error carries the supplied value:\n%v", err)
	}
	if !strings.Contains(err.Error(), "aws_secret_access_key") {
		t.Errorf("the error does not name what is missing:\n%v", err)
	}
	if !strings.Contains(err.Error(), "Nothing was escrowed") {
		t.Errorf("the error does not say that nothing was written:\n%v", err)
	}
}

// One enrolled credential, two shapes: Velero reads the INI, CloudNativePG reads three
// discrete keys. The parse must find them without the region being guessed.
func TestParseStoreCredentialRendersTheCNPGShape(t *testing.T) {
	const ini = "[default]\naws_access_key_id = AKIAEXAMPLE\naws_secret_access_key = s3cret\n"

	// Region is not in the credential, so it must come from the flag.
	if _, _, _, err := parseStoreCredential(ini, ""); err == nil {
		t.Error("a credential with no region was rendered anyway; CNPG sends the region on " +
			"every request and a wrong one reads as a missing bucket")
	} else if !strings.Contains(err.Error(), "region") {
		t.Errorf("the error does not name the region:\n%v", err)
	}

	id, secret, region, err := parseStoreCredential(ini, "hel1")
	if err != nil {
		t.Fatalf("a valid credential with --region was refused: %v", err)
	}
	if id != "AKIAEXAMPLE" || secret != "s3cret" || region != "hel1" {
		t.Errorf("parsed %q/%q/%q", id, secret, region)
	}

	// Declared in the credential, so no flag is needed.
	withRegion := ini + "region = fsn1\n"
	if _, _, r, err := parseStoreCredential(withRegion, ""); err != nil || r != "fsn1" {
		t.Errorf("a declared region was not used: %q, %v", r, err)
	}
	// The flag wins when both are present, so a one-off delivery can override.
	if _, _, r, _ := parseStoreCredential(withRegion, "hel1"); r != "hel1" {
		t.Errorf("--region did not override the declared region, got %q", r)
	}

	for name, in := range map[string]string{
		"no key id": "[default]\naws_secret_access_key=B\nregion=hel1\n",
		"no secret": "[default]\naws_access_key_id=A\nregion=hel1\n",
		"empty":     "",
	} {
		if _, _, _, err := parseStoreCredential(in, ""); err == nil {
			t.Errorf("%s was rendered anyway", name)
		}
	}

	// The error must not carry what was supplied.
	if _, _, _, err := parseStoreCredential("[default]\naws_access_key_id=LEAKME\n", ""); err != nil {
		if strings.Contains(err.Error(), "LEAKME") {
			t.Errorf("the error carries the supplied value:\n%v", err)
		}
	}
}
