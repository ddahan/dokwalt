// Package s3 is a small client for S3-compatible object storage (Cloudflare
// R2, AWS S3, MinIO, Backblaze B2…): just what off-site backups need, signed
// with AWS Signature Version 4 and the standard library only.
//
// Requests use path-style URLs (<endpoint>/<bucket>/<key>), which every
// provider above accepts. Uploads come from files: S3 needs the length (and
// here the SHA-256) of a body before sending it, so callers spool to disk.
package s3

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Files above MultipartThreshold are uploaded in PartSize parts: a single
// PUT is limited to 5 GiB, and a failed part is cheaper to retry.
var (
	MultipartThreshold int64 = 100 << 20
	PartSize           int64 = 64 << 20
)

type Client struct {
	Endpoint  string // https://<account>.r2.cloudflarestorage.com
	Region    string // "auto" for R2
	Bucket    string
	AccessKey string
	SecretKey string
	HTTP      *http.Client
	now       func() time.Time
}

// Object is one entry of a listing.
type Object struct {
	Key          string
	Size         int64
	LastModified time.Time
}

// Error is an error response from the storage provider.
type Error struct {
	Status  int
	Code    string `xml:"Code"`
	Message string `xml:"Message"`
}

func (e *Error) Error() string {
	if e.Code == "" {
		return fmt.Sprintf("storage: HTTP %d", e.Status)
	}
	return fmt.Sprintf("storage: %s: %s (HTTP %d)", e.Code, e.Message, e.Status)
}

// IsNotFound reports a missing object or bucket.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) client() *http.Client {
	if c.HTTP != nil {
		return c.HTTP
	}
	return http.DefaultClient
}

func (c *Client) objectURL(key string, q url.Values) string {
	u := strings.TrimRight(c.Endpoint, "/") + "/" + escapePath(c.Bucket)
	if key != "" {
		u += "/" + escapePath(key)
	}
	if len(q) > 0 {
		u += "?" + canonicalQuery(q)
	}
	return u
}

// do sends a signed request. body may be nil; payloadHash is the hex SHA-256
// of the body.
func (c *Client) do(ctx context.Context, method, key string, q url.Values, body io.Reader, size int64, payloadHash string, hdr http.Header) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, method, c.objectURL(key, q), body)
	if err != nil {
		return nil, err
	}
	if body != nil {
		req.ContentLength = size
	}
	for k, v := range hdr {
		req.Header[k] = v
	}
	c.sign(req, payloadHash)
	resp, err := c.client().Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 300 {
		defer resp.Body.Close()
		e := &Error{Status: resp.StatusCode}
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		_ = xml.Unmarshal(b, e)
		return nil, e
	}
	return resp, nil
}

// doSmall sends an in-memory body (or none) and returns the response body.
func (c *Client) doSmall(ctx context.Context, method, key string, q url.Values, body []byte, hdr http.Header) ([]byte, http.Header, error) {
	var r io.Reader
	if body != nil {
		r = bytes.NewReader(body)
	}
	resp, err := c.do(ctx, method, key, q, r, int64(len(body)), hashHex(body), hdr)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	return b, resp.Header, err
}

// PutFile uploads a file, in parts when it's large.
func (c *Client) PutFile(ctx context.Context, key, path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return err
	}
	if st.Size() > MultipartThreshold {
		return c.putMultipart(ctx, key, f, st.Size())
	}
	return c.putSection(ctx, key, nil, f, 0, st.Size())
}

// PutBytes uploads a small in-memory object.
func (c *Client) PutBytes(ctx context.Context, key string, b []byte) error {
	_, _, err := c.doSmall(ctx, http.MethodPut, key, nil, b, nil)
	return err
}

// putSection uploads bytes [off, off+n) of f, hashing them first.
func (c *Client) putSection(ctx context.Context, key string, q url.Values, f *os.File, off, n int64) error {
	_, err := c.putPart(ctx, key, q, f, off, n)
	return err
}

func (c *Client) putPart(ctx context.Context, key string, q url.Values, f *os.File, off, n int64) (string, error) {
	h := sha256.New()
	if _, err := io.Copy(h, io.NewSectionReader(f, off, n)); err != nil {
		return "", err
	}
	resp, err := c.do(ctx, http.MethodPut, key, q, io.NewSectionReader(f, off, n), n, hex.EncodeToString(h.Sum(nil)), nil)
	if err != nil {
		return "", err
	}
	_, _ = io.Copy(io.Discard, resp.Body)
	resp.Body.Close()
	return resp.Header.Get("ETag"), nil
}

func (c *Client) putMultipart(ctx context.Context, key string, f *os.File, size int64) error {
	b, _, err := c.doSmall(ctx, http.MethodPost, key, url.Values{"uploads": {""}}, nil, nil)
	if err != nil {
		return err
	}
	var init struct {
		UploadID string `xml:"UploadId"`
	}
	if err := xml.Unmarshal(b, &init); err != nil || init.UploadID == "" {
		return fmt.Errorf("storage: could not start a multipart upload")
	}
	type part struct {
		PartNumber int    `xml:"PartNumber"`
		ETag       string `xml:"ETag"`
	}
	var parts []part
	err = func() error {
		for off, n := int64(0), 1; off < size; off, n = off+PartSize, n+1 {
			q := url.Values{"partNumber": {strconv.Itoa(n)}, "uploadId": {init.UploadID}}
			etag, err := c.putPart(ctx, key, q, f, off, min(PartSize, size-off))
			if err != nil {
				return fmt.Errorf("part %d: %w", n, err)
			}
			parts = append(parts, part{PartNumber: n, ETag: etag})
		}
		body, _ := xml.Marshal(struct {
			XMLName xml.Name `xml:"CompleteMultipartUpload"`
			Parts   []part   `xml:"Part"`
		}{Parts: parts})
		_, _, err := c.doSmall(ctx, http.MethodPost, key, url.Values{"uploadId": {init.UploadID}}, body,
			http.Header{"Content-Type": {"application/xml"}})
		return err
	}()
	if err != nil {
		// Don't leave invisible parts behind: they're billed until aborted.
		_, _, _ = c.doSmall(context.WithoutCancel(ctx), http.MethodDelete, key, url.Values{"uploadId": {init.UploadID}}, nil, nil)
	}
	return err
}

// Get streams an object. The caller closes it.
func (c *Client) Get(ctx context.Context, key string) (io.ReadCloser, int64, error) {
	resp, err := c.do(ctx, http.MethodGet, key, nil, nil, 0, hashHex(nil), nil)
	if err != nil {
		return nil, 0, err
	}
	return resp.Body, resp.ContentLength, nil
}

// GetBytes reads a small object.
func (c *Client) GetBytes(ctx context.Context, key string) ([]byte, error) {
	b, _, err := c.doSmall(ctx, http.MethodGet, key, nil, nil, nil)
	return b, err
}

// Delete removes an object. Deleting a missing object is not an error.
func (c *Client) Delete(ctx context.Context, key string) error {
	_, _, err := c.doSmall(ctx, http.MethodDelete, key, nil, nil, nil)
	if IsNotFound(err) {
		return nil
	}
	return err
}

// List returns every object whose key starts with prefix, in key order.
func (c *Client) List(ctx context.Context, prefix string) ([]Object, error) {
	var out []Object
	token := ""
	for {
		q := url.Values{"list-type": {"2"}, "prefix": {prefix}}
		if token != "" {
			q.Set("continuation-token", token)
		}
		b, _, err := c.doSmall(ctx, http.MethodGet, "", q, nil, nil)
		if err != nil {
			return nil, err
		}
		var res struct {
			Contents []struct {
				Key          string    `xml:"Key"`
				Size         int64     `xml:"Size"`
				LastModified time.Time `xml:"LastModified"`
			} `xml:"Contents"`
			IsTruncated           bool   `xml:"IsTruncated"`
			NextContinuationToken string `xml:"NextContinuationToken"`
		}
		if err := xml.Unmarshal(b, &res); err != nil {
			return nil, fmt.Errorf("storage: unreadable listing: %w", err)
		}
		for _, o := range res.Contents {
			out = append(out, Object{Key: o.Key, Size: o.Size, LastModified: o.LastModified})
		}
		if !res.IsTruncated || res.NextContinuationToken == "" {
			return out, nil
		}
		token = res.NextContinuationToken
	}
}

// ---- Signature Version 4 ----

const emptyHash = "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

func hashHex(b []byte) string {
	if len(b) == 0 {
		return emptyHash
	}
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func hmacSHA256(key []byte, s string) []byte {
	m := hmac.New(sha256.New, key)
	m.Write([]byte(s))
	return m.Sum(nil)
}

// sign adds the x-amz-* headers and the Authorization header. It signs the
// host and every header already set on the request.
func (c *Client) sign(req *http.Request, payloadHash string) {
	now := time.Now
	if c.now != nil {
		now = c.now
	}
	t := now().UTC()
	amzDate := t.Format("20060102T150405Z")
	day := t.Format("20060102")
	req.Header.Set("X-Amz-Date", amzDate)
	req.Header.Set("X-Amz-Content-Sha256", payloadHash)

	headers := map[string]string{"host": req.URL.Host}
	for k, v := range req.Header {
		headers[strings.ToLower(k)] = strings.TrimSpace(strings.Join(v, ","))
	}
	names := make([]string, 0, len(headers))
	for k := range headers {
		names = append(names, k)
	}
	sort.Strings(names)
	var canonHeaders strings.Builder
	for _, k := range names {
		canonHeaders.WriteString(k + ":" + headers[k] + "\n")
	}
	signed := strings.Join(names, ";")

	path := req.URL.EscapedPath()
	if path == "" {
		path = "/"
	}
	canonReq := strings.Join([]string{req.Method, path, canonicalQuery(req.URL.Query()), canonHeaders.String(), signed, payloadHash}, "\n")
	region := c.Region
	if region == "" {
		region = "auto"
	}
	scope := day + "/" + region + "/s3/aws4_request"
	toSign := "AWS4-HMAC-SHA256\n" + amzDate + "\n" + scope + "\n" + hashHex([]byte(canonReq))
	key := hmacSHA256([]byte("AWS4"+c.SecretKey), day)
	key = hmacSHA256(key, region)
	key = hmacSHA256(key, "s3")
	key = hmacSHA256(key, "aws4_request")
	sig := hex.EncodeToString(hmacSHA256(key, toSign))
	req.Header.Set("Authorization", fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s", c.AccessKey, scope, signed, sig))
}

// uriEncode escapes everything but RFC 3986 unreserved characters, as SigV4
// requires (url.QueryEscape turns spaces into '+', which S3 rejects).
func uriEncode(s string, keepSlash bool) string {
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		ch := s[i]
		switch {
		case 'A' <= ch && ch <= 'Z', 'a' <= ch && ch <= 'z', '0' <= ch && ch <= '9',
			ch == '-', ch == '_', ch == '.', ch == '~', keepSlash && ch == '/':
			b.WriteByte(ch)
		default:
			fmt.Fprintf(&b, "%%%02X", ch)
		}
	}
	return b.String()
}

func escapePath(s string) string { return uriEncode(s, true) }

func canonicalQuery(q url.Values) string {
	keys := make([]string, 0, len(q))
	for k := range q {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var parts []string
	for _, k := range keys {
		vs := append([]string(nil), q[k]...)
		sort.Strings(vs)
		for _, v := range vs {
			parts = append(parts, uriEncode(k, false)+"="+uriEncode(v, false))
		}
	}
	return strings.Join(parts, "&")
}
