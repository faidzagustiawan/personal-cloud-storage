// Package config loads runtime configuration from the environment.
//
// Everything has a working default except the B2 credentials, which are only
// required from Step 2 onward. Nothing is read from a file the repository
// tracks — secrets live in /etc/cloudapp/env, mode 0600 (spec §7.5).
package config

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"
)

type Config struct {
	// Server
	Addr         string
	DBPath       string
	Origins      []string // exact origins accepted on mutating requests (CSRF check, spec §8)
	SecureCookie bool     // Secure attribute on the session cookie; false only for plain-HTTP local dev
	TrustedProxy bool     // honour X-Forwarded-For, correct when Nginx sits in front

	// TempDir is where the nightly database copy is written before upload.
	// Kept configurable because systemd's PrivateTmp puts it somewhere specific.
	TempDir string

	// Sessions
	SessionTTL time.Duration

	// Accounts
	DefaultQuota int64

	// Backblaze B2 (Step 2)
	B2KeyID       string
	B2AppKey      string
	B2UploadKeyID string
	B2UploadKey   string
	B2BucketID    string
	B2BucketName  string
	CDNBase       string

	// B2APIBase is the b2_authorize_account host. Overridable so tests can
	// point the whole stack at a fake B2, and so a non-default B2 realm can be
	// reached without a code change.
	B2APIBase string

	// AllowMasterUpload lets browser upload tokens be minted from the master
	// key when no restricted key exists. Development only: it hands a client
	// full bucket access and is logged on every use.
	AllowMasterUpload bool
}

func Load() (*Config, error) {
	c := &Config{
		Addr:         str("CLOUD_ADDR", "127.0.0.1:8080"),
		DBPath:       str("CLOUD_DB", "cloud.db"),
		TempDir:      str("CLOUD_TEMP", ""),
		SessionTTL:   time.Duration(num("CLOUD_SESSION_DAYS", 30)) * 24 * time.Hour,
		DefaultQuota: num("CLOUD_DEFAULT_QUOTA", 100<<30), // 100 GB, decision D9

		B2KeyID:       str("B2_KEY_ID", ""),
		B2AppKey:      str("B2_APP_KEY", ""),
		B2UploadKeyID: str("B2_UPLOAD_KEY_ID", ""),
		B2UploadKey:   str("B2_UPLOAD_APP_KEY", ""),
		B2BucketID:    str("B2_BUCKET_ID", ""),
		B2BucketName:  str("B2_BUCKET_NAME", ""),
		CDNBase:       strings.TrimRight(str("B2_CDN_BASE", ""), "/"),
		B2APIBase:     strings.TrimRight(str("B2_API_BASE", ""), "/"),

		AllowMasterUpload: boolean("B2_ALLOW_MASTER_UPLOAD", false),
	}

	for _, o := range strings.Split(str("CLOUD_ORIGINS", "http://localhost:5173"), ",") {
		if o = strings.TrimSpace(strings.TrimRight(o, "/")); o != "" {
			c.Origins = append(c.Origins, o)
		}
	}
	if len(c.Origins) == 0 {
		return nil, fmt.Errorf("CLOUD_ORIGINS is empty: every mutating request would be rejected")
	}

	// Default to secure cookies unless every configured origin is plain-HTTP
	// localhost. Getting this backwards in production silently exposes the
	// session cookie, so it is derived rather than left to a forgotten flag.
	c.SecureCookie = boolean("CLOUD_SECURE_COOKIE", !allLocalHTTP(c.Origins))
	c.TrustedProxy = boolean("CLOUD_TRUSTED_PROXY", true)

	return c, nil
}

// PublicOrigin is the origin share links are built against: the first entry in
// CLOUD_ORIGINS, which is the canonical one.
func (c *Config) PublicOrigin() string {
	if len(c.Origins) > 0 {
		return c.Origins[0]
	}
	return ""
}

// B2Configured reports whether the storage layer can be used. Step 1 runs
// without it; the health endpoint says so rather than pretending otherwise.
func (c *Config) B2Configured() bool {
	return c.B2KeyID != "" && c.B2AppKey != "" && c.B2BucketID != "" && c.B2BucketName != ""
}

func allLocalHTTP(origins []string) bool {
	for _, o := range origins {
		if !strings.HasPrefix(o, "http://localhost") && !strings.HasPrefix(o, "http://127.0.0.1") {
			return false
		}
	}
	return true
}

func str(key, def string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return def
}

func num(key string, def int64) int64 {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if n, err := strconv.ParseInt(v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

func boolean(key string, def bool) bool {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		if b, err := strconv.ParseBool(v); err == nil {
			return b
		}
	}
	return def
}
