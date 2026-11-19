package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// AWS's published presigned-URL example (S3 docs, "Authenticating requests:
// using query parameters"). If our signer reproduces its signature, the
// SigV4 maths is right.
func TestSignMatchesAWSExample(t *testing.T) {
	got := sign(signParams{
		method:    "GET",
		host:      "examplebucket.s3.amazonaws.com",
		path:      "/test.txt",
		region:    "us-east-1",
		accessKey: "AKIAIOSFODNN7EXAMPLE",
		secretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now:       time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC),
		expires:   86400 * time.Second,
	})
	const want = "aeeed9bbccd4d02ee5c0109b86d86835f995330da4c265957d157751f604d404"
	if !strings.HasSuffix(got, "X-Amz-Signature="+want) {
		t.Fatalf("signature mismatch:\n got %s\nwant ...%s", got, want)
	}
}

func TestPresignPutShape(t *testing.T) {
	s, err := New(Config{AccountID: "acc123", Bucket: "snappyy", AccessKeyID: "k", SecretAccessKey: "s", PublicBaseURL: "https://cdn.example.com/"})
	if err != nil {
		t.Fatal(err)
	}
	u := s.PresignPut("products_images/u1/abc.png", "image/png", 5*time.Minute)
	for _, part := range []string{
		"https://acc123.r2.cloudflarestorage.com/snappyy/products_images/u1/abc.png?",
		"X-Amz-SignedHeaders=content-type%3Bhost",
		"X-Amz-Expires=300",
		"X-Amz-Content-Sha256=UNSIGNED-PAYLOAD",
		"X-Amz-Signature=",
	} {
		if !strings.Contains(u, part) {
			t.Errorf("url missing %q:\n%s", part, u)
		}
	}
	if got := s.PublicURL("a/b.png"); got != "https://cdn.example.com/a/b.png" {
		t.Errorf("PublicURL = %s", got)
	}
	if !s.OwnsURL("https://cdn.example.com/x.png") || s.OwnsURL("https://evil.com/x.png") {
		t.Error("OwnsURL wrong")
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	if _, err := New(Config{Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", PublicBaseURL: "https://x.com"}); err == nil {
		t.Error("missing account id should fail")
	}
	if _, err := New(Config{AccountID: "a", Bucket: "b", AccessKeyID: "k", SecretAccessKey: "s", PublicBaseURL: "cdn.example.com"}); err == nil {
		t.Error("public url without scheme should fail")
	}
}

func TestBuildKey(t *testing.T) {
	k, err := BuildKey(Kinds["product"], "65f/../a1", "png")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(k, "products_images/65fa1/") || !strings.HasSuffix(k, ".png") {
		t.Errorf("key = %s", k)
	}
	if _, err := BuildKey(Kinds["product"], "../", "png"); err == nil {
		t.Error("empty owner after sanitising should fail")
	}
}

func TestValidKeyRejectsTraversalAndOddShapes(t *testing.T) {
	good := "products_images/abc123/0123456789abcdef0123456789abcdef.webp"
	if !ValidKey(good) {
		t.Fatal("generated-shape key rejected")
	}
	h := "0123456789abcdef0123456789abcdef"
	for _, k := range []string{
		"", "a/b", "a/b/c/d/" + h + ".webp", "../x/" + h + ".webp", "products_images/../" + h + ".webp",
		"products_images/%2e%2e/" + h + ".webp", "products_images/abc/" + h + ".html", "products_images/abc/" + h + ".svg",
		"products_images/abc/" + h + ".webp?x=1", "products_images/abc/" + h + ".webp#f", "products_images\\abc/" + h + ".webp",
		"products_images/ab.c/" + h + ".webp", "products_images//" + h + ".webp", "products_images/abc/short.webp",
		"products_images/abc/" + h + ".webp\x00.php", "products_images/abc/" + h + "X.webp", "/products_images/abc/" + h + ".webp",
	} {
		if ValidKey(k) {
			t.Errorf("accepted bad key %q", k)
		}
	}
}

func TestKeyFromURLOnlyOwnBucket(t *testing.T) {
	r, _ := New(Config{AccountID: "acct", Bucket: "b", AccessKeyID: "AK", SecretAccessKey: "SK", PublicBaseURL: "https://cdn.example.com"})
	h := "0123456789abcdef0123456789abcdef"
	if k, ok := r.KeyFromURL("https://cdn.example.com/products_images/u1/" + h + ".webp"); !ok || k != "products_images/u1/"+h+".webp" {
		t.Fatal("own url rejected")
	}
	for _, u := range []string{
		"https://evil.example.com/products_images/u1/" + h + ".webp",
		"https://cdn.example.com.evil.com/products_images/u1/" + h + ".webp",
		"https://cdn.example.com/../products_images/u1/" + h + ".webp",
		"https://cdn.example.com/products_images/u1/" + h + ".webp?x=1",
		"data:image/png;base64,AAAA",
	} {
		if _, ok := r.KeyFromURL(u); ok {
			t.Errorf("accepted %q", u)
		}
	}
}

func TestPutObjectIsSignedAndLeaksNothing(t *testing.T) {
	var gotCT, gotCC, gotQuery string
	var gotBody []byte
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotCT, gotCC, gotQuery = r.Header.Get("Content-Type"), r.Header.Get("Cache-Control"), r.URL.RawQuery
		gotBody, _ = io.ReadAll(r.Body)
		if r.URL.Query().Get("X-Amz-Signature") == "" {
			w.WriteHeader(403)
			return
		}
		w.WriteHeader(200)
	}))
	defer srv.Close()
	r, _ := New(Config{AccountID: "acct", Bucket: "b", AccessKeyID: "AK", SecretAccessKey: "TOPSECRET", PublicBaseURL: "https://cdn.example.com"})
	r.rewrite = func(u string) string { return srv.URL + u[len("https://"+r.host):] }
	key, _ := BuildKey(Kinds["product"], "u1", "webp")
	if err := r.PutObject(context.Background(), key, "image/webp", []byte("data")); err != nil {
		t.Fatal(err)
	}
	if gotCT != "image/webp" || gotCC != ImmutableCache || string(gotBody) != "data" {
		t.Fatalf("ct=%q cc=%q body=%q", gotCT, gotCC, gotBody)
	}
	if !strings.Contains(gotQuery, "content-type") || !strings.Contains(gotQuery, "cache-control") {
		t.Fatalf("content-type/cache-control not signed: %s", gotQuery)
	}
	if strings.Contains(gotQuery, "TOPSECRET") {
		t.Fatal("secret key in URL")
	}
	if err := r.PutObject(context.Background(), "../../etc/passwd", "image/webp", nil); err == nil {
		t.Fatal("traversal key accepted by PutObject")
	}
	// Failure message must not contain the signed URL.
	srv.Close()
	err := r.PutObject(context.Background(), key, "image/webp", []byte("x"))
	if err == nil || strings.Contains(err.Error(), "X-Amz-Signature") || strings.Contains(err.Error(), "AK/") {
		t.Fatalf("error leaks signed URL: %v", err)
	}
}
