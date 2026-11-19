package config

import (
	"strings"
	"testing"
	"time"
)

func lookupFrom(m map[string]string) Lookup {
	return func(k string) (string, bool) { v, ok := m[k]; return v, ok }
}

func validEnv() map[string]string {
	return map[string]string{
		"MONGODB_URI": "mongodb://localhost:27017",
		"MONGODB_DB":  "shopbd",
		"JWT_SECRET":  strings.Repeat("s3cr3t-value-", 4),
	}
}

func TestValidConfigWithDefaults(t *testing.T) {
	c, err := FromLookup(lookupFrom(validEnv()))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if c.Port != "8080" || c.Env != "development" {
		t.Errorf("defaults wrong: port=%s env=%s", c.Port, c.Env)
	}
	if c.AccessTTL != 15*time.Minute || c.RefreshTTL != 720*time.Hour {
		t.Errorf("ttl defaults wrong: %v %v", c.AccessTTL, c.RefreshTTL)
	}
	if len(c.CORSOrigins) != 1 || c.CORSOrigins[0] != "http://localhost:3000" {
		t.Errorf("cors default wrong: %v", c.CORSOrigins)
	}
	if !c.AutoCreateIndexes || c.CookieSecure || c.TrustProxy {
		t.Errorf("bool defaults wrong: %+v", c)
	}
}

func TestMissingRequiredValues(t *testing.T) {
	_, err := FromLookup(lookupFrom(map[string]string{}))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, want := range []string{"MONGODB_URI is required", "MONGODB_DB is required", "JWT_SECRET is required"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should contain %q, got %q", want, err.Error())
		}
	}
}

func TestRejectsPlaceholdersAndWeakSecrets(t *testing.T) {
	cases := map[string]map[string]string{
		"short secret":         {"JWT_SECRET": "short"},
		"placeholder secret":   {"JWT_SECRET": "<paste-generated-secret-here-xxxxxxxxxxxx>"},
		"change_me secret":     {"JWT_SECRET": "CHANGE_ME_please_replace_this_value_now"},
		"placeholder mongo":    {"MONGODB_URI": "mongodb+srv://<username>:<password>@<cluster-host>/"},
		"bad mongo scheme":     {"MONGODB_URI": "postgres://x"},
		"bad db name":          {"MONGODB_DB": "my.db"},
		"wildcard cors":        {"CORS_ALLOWED_ORIGINS": "*"},
		"cors with path":       {"CORS_ALLOWED_ORIGINS": "http://localhost:3000/app"},
		"cors bad scheme":      {"CORS_ALLOWED_ORIGINS": "ftp://localhost"},
		"bad access ttl":       {"ACCESS_TOKEN_TTL": "soon"},
		"access ttl too long":  {"ACCESS_TOKEN_TTL": "5h"},
		"refresh below access": {"ACCESS_TOKEN_TTL": "30m", "REFRESH_TOKEN_TTL": "10m"},
		"bad port":             {"PORT": "99999"},
		"bad bool":             {"COOKIE_SECURE": "maybe"},
		"bad env":              {"APP_ENV": "staging"},
	}
	for name, override := range cases {
		t.Run(name, func(t *testing.T) {
			env := validEnv()
			for k, v := range override {
				env[k] = v
			}
			if _, err := FromLookup(lookupFrom(env)); err == nil {
				t.Fatal("expected a validation error")
			}
		})
	}
}

func TestErrorsNeverContainSecrets(t *testing.T) {
	env := validEnv()
	env["JWT_SECRET"] = "tooshort-secret-value"
	env["MONGODB_URI"] = "mongodb://user:hunter2@localhost/<x>"
	_, err := FromLookup(lookupFrom(env))
	if err == nil {
		t.Fatal("expected error")
	}
	for _, leaked := range []string{"tooshort-secret-value", "hunter2"} {
		if strings.Contains(err.Error(), leaked) {
			t.Errorf("error leaks secret value %q: %v", leaked, err)
		}
	}
}

func TestProductionRequiresSecureCookies(t *testing.T) {
	env := validEnv()
	env["APP_ENV"] = "production"
	if _, err := FromLookup(lookupFrom(env)); err == nil {
		t.Fatal("production without COOKIE_SECURE should fail")
	}
	env["COOKIE_SECURE"] = "true"
	if _, err := FromLookup(lookupFrom(env)); err != nil {
		t.Fatalf("production with COOKIE_SECURE should pass: %v", err)
	}
}

func TestOriginsAreNormalized(t *testing.T) {
	env := validEnv()
	env["CORS_ALLOWED_ORIGINS"] = " HTTP://LocalHost:3000/ , https://shop.example.com "
	c, err := FromLookup(lookupFrom(env))
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"http://localhost:3000", "https://shop.example.com"}
	if len(c.CORSOrigins) != 2 || c.CORSOrigins[0] != want[0] || c.CORSOrigins[1] != want[1] {
		t.Errorf("got %v want %v", c.CORSOrigins, want)
	}
}
