package s3

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"
)

// The "GET Object" example of the AWS Signature Version 4 documentation.
func TestSignMatchesAWSExample(t *testing.T) {
	c := &Client{
		Endpoint: "https://examplebucket.s3.amazonaws.com", Region: "us-east-1",
		AccessKey: "AKIAIOSFODNN7EXAMPLE", SecretKey: "wJalrXUtnFEMI/K7MDENG/bPxRfiCYEXAMPLEKEY",
		now: func() time.Time { return time.Date(2013, 5, 24, 0, 0, 0, 0, time.UTC) },
	}
	req, _ := http.NewRequest("GET", "https://examplebucket.s3.amazonaws.com/test.txt", nil)
	req.Header.Set("Range", "bytes=0-9")
	c.sign(req, emptyHash)
	want := "AWS4-HMAC-SHA256 Credential=AKIAIOSFODNN7EXAMPLE/20130524/us-east-1/s3/aws4_request, " +
		"SignedHeaders=host;range;x-amz-content-sha256;x-amz-date, " +
		"Signature=f0e8bdb87c964420e857bd35b5d6ed310bd44f0170aba48dd91039c6036bdb41"
	if got := req.Header.Get("Authorization"); got != want {
		t.Fatalf("Authorization\n got %s\nwant %s", got, want)
	}
}

func TestURIEncode(t *testing.T) {
	if got := escapePath("a b/c+d~é"); got != "a%20b/c%2Bd~%C3%A9" {
		t.Fatalf("got %s", got)
	}
	if got := canonicalQuery(map[string][]string{"prefix": {"x/y"}, "list-type": {"2"}, "uploads": {""}}); got != "list-type=2&prefix=x%2Fy&uploads=" {
		t.Fatalf("got %s", got)
	}
}

// fakeS3 is an in-memory bucket that checks every request is signed.
type fakeS3 struct {
	mu       sync.Mutex
	objects  map[string][]byte
	parts    map[string]map[string][]byte // uploadId → part number → data
	pageSize int
	aborted  int
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !strings.HasPrefix(r.Header.Get("Authorization"), "AWS4-HMAC-SHA256 Credential=key/") || r.Header.Get("X-Amz-Content-Sha256") == "" {
		w.WriteHeader(http.StatusForbidden)
		fmt.Fprint(w, "<Error><Code>AccessDenied</Code><Message>unsigned</Message></Error>")
		return
	}
	key := strings.TrimPrefix(r.URL.Path, "/bucket")
	key = strings.TrimPrefix(key, "/")
	q := r.URL.Query()
	body, _ := io.ReadAll(r.Body)
	switch {
	case r.Method == "GET" && key == "":
		var keys []string
		for k := range f.objects {
			if strings.HasPrefix(k, q.Get("prefix")) && k > q.Get("continuation-token") {
				keys = append(keys, k)
			}
		}
		sort.Strings(keys)
		truncated := len(keys) > f.pageSize
		if truncated {
			keys = keys[:f.pageSize]
		}
		fmt.Fprint(w, "<ListBucketResult>")
		for _, k := range keys {
			fmt.Fprintf(w, "<Contents><Key>%s</Key><Size>%d</Size><LastModified>2026-10-10T03:00:00.000Z</LastModified></Contents>", k, len(f.objects[k]))
		}
		if truncated {
			fmt.Fprintf(w, "<IsTruncated>true</IsTruncated><NextContinuationToken>%s</NextContinuationToken>", keys[len(keys)-1])
		}
		fmt.Fprint(w, "</ListBucketResult>")
	case r.Method == "POST" && q.Has("uploads"):
		id := fmt.Sprintf("up%d", len(f.parts)+1)
		f.parts[id] = map[string][]byte{}
		fmt.Fprintf(w, "<InitiateMultipartUploadResult><UploadId>%s</UploadId></InitiateMultipartUploadResult>", id)
	case r.Method == "PUT" && q.Has("uploadId"):
		f.parts[q.Get("uploadId")][q.Get("partNumber")] = body
		w.Header().Set("ETag", `"etag-`+q.Get("partNumber")+`"`)
	case r.Method == "POST" && q.Has("uploadId"):
		var all []byte
		parts := f.parts[q.Get("uploadId")]
		for i := 1; i <= len(parts); i++ {
			if !strings.Contains(string(body), fmt.Sprintf(`<PartNumber>%d</PartNumber><ETag>&#34;etag-%d&#34;</ETag>`, i, i)) {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			all = append(all, parts[fmt.Sprint(i)]...)
		}
		f.objects[key] = all
	case r.Method == "DELETE" && q.Has("uploadId"):
		f.aborted++
	case r.Method == "PUT":
		f.objects[key] = body
	case r.Method == "GET":
		b, ok := f.objects[key]
		if !ok {
			w.WriteHeader(http.StatusNotFound)
			fmt.Fprint(w, "<Error><Code>NoSuchKey</Code><Message>The specified key does not exist.</Message></Error>")
			return
		}
		_, _ = w.Write(b)
	case r.Method == "DELETE":
		delete(f.objects, key)
		w.WriteHeader(http.StatusNoContent)
	}
}

func newFake(t *testing.T) (*Client, *fakeS3) {
	f := &fakeS3{objects: map[string][]byte{}, parts: map[string]map[string][]byte{}, pageSize: 2}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	return &Client{Endpoint: srv.URL, Bucket: "bucket", AccessKey: "key", SecretKey: "secret"}, f
}

func writeFile(t *testing.T, data string) string {
	p := filepath.Join(t.TempDir(), "f")
	if err := os.WriteFile(p, []byte(data), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPutGetListDelete(t *testing.T) {
	c, _ := newFake(t)
	ctx := context.Background()
	for _, k := range []string{"h/1/a.dump", "h/1/manifest.json", "h/2/a.dump", "other/x"} {
		if err := c.PutFile(ctx, k, writeFile(t, "data of "+k)); err != nil {
			t.Fatal(err)
		}
	}
	objs, err := c.List(ctx, "h/")
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, o := range objs {
		keys = append(keys, o.Key)
	}
	if strings.Join(keys, ",") != "h/1/a.dump,h/1/manifest.json,h/2/a.dump" {
		t.Fatalf("listing across pages: %v", keys)
	}
	b, err := c.GetBytes(ctx, "h/2/a.dump")
	if err != nil || string(b) != "data of h/2/a.dump" {
		t.Fatalf("get: %q %v", b, err)
	}
	if err := c.Delete(ctx, "h/2/a.dump"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.GetBytes(ctx, "h/2/a.dump"); !IsNotFound(err) {
		t.Fatalf("want not found, got %v", err)
	}
}

func TestMultipart(t *testing.T) {
	defer func(th, ps int64) { MultipartThreshold, PartSize = th, ps }(MultipartThreshold, PartSize)
	MultipartThreshold, PartSize = 10, 4
	c, f := newFake(t)
	if err := c.PutFile(context.Background(), "big", writeFile(t, "0123456789abcdefghij!")); err != nil {
		t.Fatal(err)
	}
	if got := string(f.objects["big"]); got != "0123456789abcdefghij!" {
		t.Fatalf("reassembled %q", got)
	}
	if n := len(f.parts["up1"]); n != 6 {
		t.Fatalf("want 6 parts, got %d", n)
	}
}

func TestErrorIsReadable(t *testing.T) {
	c, _ := newFake(t)
	c.AccessKey = "wrong"
	err := c.PutBytes(context.Background(), "x", []byte("y"))
	if err == nil || !strings.Contains(err.Error(), "AccessDenied") {
		t.Fatalf("got %v", err)
	}
}
