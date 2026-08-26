package goo

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

// Validation errors returned when untrusted bucket/key input is rejected.
var (
	ErrInvalidBucket = errors.New("goo: invalid bucket name")
	ErrInvalidKey    = errors.New("goo: invalid object key")
)

// Bucket names follow a conservative S3-like rule: 3-63 lowercase alphanumerics
// and dashes/underscores. This keeps on-disk directory names safe and portable.
var bucketRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{1,61}[a-z0-9]$`)

// windowsReserved are bucket names that cannot be top-level directories on
// Windows (case-insensitive). We reject them everywhere so a bucket created on
// Linux doesn't become unmovable to a Windows host.
var windowsReserved = map[string]struct{}{
	"con": {}, "prn": {}, "aux": {}, "nul": {},
	"com1": {}, "com2": {}, "com3": {}, "com4": {}, "com5": {}, "com6": {}, "com7": {}, "com8": {}, "com9": {},
	"lpt1": {}, "lpt2": {}, "lpt3": {}, "lpt4": {}, "lpt5": {}, "lpt6": {}, "lpt7": {}, "lpt8": {}, "lpt9": {},
}

// ValidBucket reports whether name is a safe bucket name. Bucket names become
// top-level directories on disk, so we reject anything that could be a path
// component attack (slashes, dots, etc.).
func ValidBucket(name string) bool {
	if name == "" || len(name) > 63 {
		return false
	}
	// reject Windows reserved device names even with no extension.
	if _, reserved := windowsReserved[strings.ToLower(name)]; reserved {
		return false
	}
	return bucketRe.MatchString(name)
}

// ValidKey reports whether key is a safe object key. We allow slashes so that
// keys can be hierarchical, but we forbid path-traversal and absolute paths.
//
// The rules are deliberately strict and explicit:
//   - no empty key
//   - no leading or trailing slash
//   - no ".." path segments (the classic traversal trick)
//   - no NUL byte
//   - no leading "./" or "/"
//   - only printable, reasonable characters
func ValidKey(key string) bool {
	if key == "" || len(key) > 1024 {
		return false
	}
	if strings.HasPrefix(key, "/") || strings.HasSuffix(key, "/") {
		return false
	}
	if strings.ContainsRune(key, '\x00') {
		return false
	}
	for _, seg := range strings.Split(key, "/") {
		if seg == "" {
			return false // double slash or stray slash
		}
		if seg == "." || seg == ".." {
			return false // traversal attempt ("." and ".." segments)
		}
	}
	// forbid anything that isn't a sane filename byte.
	if !keyRe.MatchString(key) {
		return false
	}
	return true
}

// keyRe allows hierarchical keys with common safe characters. We intentionally
// exclude spaces, backslashes, and control characters.
var keyRe = regexp.MustCompile(`^[A-Za-z0-9._\-/]+$`)

// CheckRef validates a bucket/key pair and returns a descriptive error.
func CheckRef(bucket, key string) error {
	if !ValidBucket(bucket) {
		return fmt.Errorf("%w: %q", ErrInvalidBucket, bucket)
	}
	if !ValidKey(key) {
		return fmt.Errorf("%w: %q", ErrInvalidKey, key)
	}
	return nil
}
