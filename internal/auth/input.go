package auth

import (
	"fmt"
	"io"
	"strings"

	"golang.org/x/term"
)

type fileDescriptor interface {
	Fd() uintptr
}

// ReadToken reads one bounded token. Terminal input is read without echo.
func ReadToken(input io.Reader) (Credential, error) {
	if input == nil {
		return Credential{}, fmt.Errorf("%w: no input", ErrInvalidToken)
	}
	var raw []byte
	var err error
	if source, ok := input.(fileDescriptor); ok && term.IsTerminal(int(source.Fd())) {
		raw, err = term.ReadPassword(int(source.Fd()))
	} else {
		raw, err = io.ReadAll(io.LimitReader(input, MaxTokenBytes+2))
	}
	if err != nil {
		return Credential{}, fmt.Errorf("read token: %w", err)
	}
	if len(raw) > MaxTokenBytes+1 {
		return Credential{}, fmt.Errorf("%w: token exceeds %d bytes", ErrInvalidToken, MaxTokenBytes)
	}
	token := string(raw)
	if strings.HasSuffix(token, "\r\n") {
		token = strings.TrimSuffix(token, "\r\n")
	} else {
		token = strings.TrimSuffix(token, "\n")
	}
	credential := Credential{Token: token}
	if err := credential.Validate(); err != nil {
		return Credential{}, err
	}
	return credential, nil
}
