package config

import (
	"errors"
	"fmt"
	"os"
	"strings"
)

// SecretRef points at a secret value. Exactly one of Value, Env or File must be
// set. File is re-read on every Resolve so mounted secrets can rotate without a
// restart; callers are expected to cache whatever they derive from it.
type SecretRef struct {
	Value string `json:"value,omitempty"`
	Env   string `json:"env,omitempty"`
	File  string `json:"file,omitempty"`
}

func (s *SecretRef) IsSet() bool {
	return s != nil && (s.Value != "" || s.Env != "" || s.File != "")
}

func (s *SecretRef) validate(field string) error {
	if s == nil {
		return fmt.Errorf("%s: secret is required", field)
	}
	n := 0
	for _, v := range []string{s.Value, s.Env, s.File} {
		if v != "" {
			n++
		}
	}
	if n != 1 {
		return fmt.Errorf("%s: exactly one of value, env or file must be set", field)
	}
	return nil
}

// Resolve returns the secret value with surrounding whitespace removed.
func (s *SecretRef) Resolve() (string, error) {
	if s == nil {
		return "", errors.New("secret not configured")
	}
	switch {
	case s.Value != "":
		return s.Value, nil
	case s.Env != "":
		v, ok := os.LookupEnv(s.Env)
		if !ok || strings.TrimSpace(v) == "" {
			return "", fmt.Errorf("environment variable %s is empty or unset", s.Env)
		}
		return strings.TrimSpace(v), nil
	case s.File != "":
		b, err := os.ReadFile(s.File)
		if err != nil {
			return "", fmt.Errorf("read secret file: %w", err)
		}
		v := strings.TrimSpace(string(b))
		if v == "" {
			return "", fmt.Errorf("secret file %s is empty", s.File)
		}
		return v, nil
	}
	return "", errors.New("secret not configured")
}

// String never prints the secret itself.
func (s SecretRef) String() string {
	switch {
	case s.Value != "":
		return "<inline secret>"
	case s.Env != "":
		return "env:" + s.Env
	case s.File != "":
		return "file:" + s.File
	}
	return "<unset>"
}
