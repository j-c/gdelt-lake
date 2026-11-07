package source

import (
	"archive/zip"
	"context"
	"crypto/md5"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

var (
	// ErrMissing means GDELT has no such file (HTTP 404).
	ErrMissing = errors.New("not published")
	// ErrCorrupt means the file arrived but its size, MD5 or zip structure is wrong.
	ErrCorrupt = errors.New("corrupt")
)

// Client talks to GDELT's file server. The zero value isn't usable; use NewClient.
type Client struct {
	baseURL   string
	http      *http.Client
	userAgent string
	attempts  int
	backoff   time.Duration // first retry delay; doubles each time
	sleep     func(context.Context, time.Duration) error
}

// NewClient returns a client for the file server at baseURL (ending in "/").
func NewClient(baseURL, userAgent string) *Client {
	return &Client{
		baseURL:   baseURL,
		http:      &http.Client{Timeout: 5 * time.Minute},
		userAgent: userAgent,
		attempts:  4,
		backoff:   5 * time.Second,
		sleep:     sleepCtx,
	}
}

// WithBackoff sets the first retry delay (it doubles on each retry) and returns the client.
func (c *Client) WithBackoff(d time.Duration) *Client {
	c.backoff = d
	return c
}

// Latest returns the files in a stream's latest batch.
func (c *Client) Latest(ctx context.Context, s Stream) ([]File, error) {
	_, latest := s.lists()
	body, _, err := c.get(ctx, latest, "")
	if err != nil {
		return nil, fmt.Errorf("%s: %w", latest, err)
	}
	return parseList(string(body), false), nil
}

// bytesPerBatch is roughly how much of a master list one batch takes (three lines), with a margin.
const bytesPerBatch = 3 * 130

// maxTail caps how much of a master list one call reads (about 2 years of batches).
const maxTail = 32 << 20

// Since returns a stream's files for batches at or after since, read from the end of its master list.
func (c *Client) Since(ctx context.Context, s Stream, since time.Time) ([]File, error) {
	master, _ := s.lists()
	batches := int64(time.Since(since)/BatchInterval) + 8
	tail := max(batches*bytesPerBatch, 64<<10)
	for {
		body, whole, err := c.get(ctx, master, "bytes=-"+strconv.FormatInt(tail, 10))
		if err != nil {
			return nil, fmt.Errorf("%s: %w", master, err)
		}
		files := parseList(string(body), !whole)
		if whole || tail >= maxTail || (len(files) > 0 && !files[0].Batch.After(since)) {
			var out []File
			for _, f := range files {
				if !f.Batch.Before(since) {
					out = append(out, f)
				}
			}
			return out, nil
		}
		tail *= 2
	}
}

// Between returns a stream's files for batches in [from, to), found in its master list with a binary search over byte
// offsets (the list is in time order), so only a few small ranges of the 130 MB list are read (design 8.5).
func (c *Client) Between(ctx context.Context, s Stream, from, to time.Time) ([]File, error) {
	master, _ := s.lists()
	_, total, err := c.rangeRead(ctx, master, 0, 0)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", master, err)
	}
	const probe, close = 4 << 10, 128 << 10
	lo, hi := int64(0), total
	for hi-lo > close {
		mid := lo + (hi-lo)/2
		body, _, err := c.rangeRead(ctx, master, mid, mid+probe-1)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", master, err)
		}
		files := parseList(string(body), true)
		if len(files) == 0 {
			lo = mid // nothing parseable here (e.g. a run of malformed lines): look later
			continue
		}
		if files[0].Batch.Before(from) {
			lo = mid
		} else {
			hi = mid
		}
	}
	// Read forward from lo, a line-aligned chunk at a time, until past "to".
	var out []File
	off, skipFirst := lo, lo > 0 // a probe offset usually lands mid-line; that line is before "from" anyway
	for off < total {
		end := min(off+close, total)
		body, _, err := c.rangeRead(ctx, master, off, end-1)
		if err != nil {
			return nil, fmt.Errorf("%s: %w", master, err)
		}
		text := string(body)
		last := strings.LastIndexByte(text, '\n')
		if end < total {
			if last < 0 {
				return nil, fmt.Errorf("%s: a line longer than %d bytes", master, close)
			}
			text = text[:last+1] // the cut line is read whole by the next chunk
		}
		for _, f := range parseList(text, skipFirst) {
			if !f.Batch.Before(to) {
				return out, nil
			}
			if !f.Batch.Before(from) {
				out = append(out, f)
			}
		}
		if end >= total {
			break
		}
		off, skipFirst = off+int64(last)+1, false
	}
	return out, nil
}

// rangeRead reads bytes [start, end] of a file and returns them with the file's total size.
func (c *Client) rangeRead(ctx context.Context, name string, start, end int64) ([]byte, int64, error) {
	body, _, total, err := c.getRange(ctx, name, fmt.Sprintf("bytes=%d-%d", start, end))
	return body, total, err
}

// Download fetches f into dir and checks its size and MD5. It returns the file's path, ErrMissing when GDELT doesn't
// have it, or ErrCorrupt when the content is wrong. Network errors and server errors are retried with backoff.
func (c *Client) Download(ctx context.Context, f File, dir string) (string, error) {
	dest := filepath.Join(dir, f.Name)
	var lastErr error
	for attempt := range c.attempts {
		if attempt > 0 {
			if err := c.sleep(ctx, c.backoff<<(attempt-1)); err != nil {
				return "", err
			}
		}
		err := c.downloadOnce(ctx, f, dest)
		if err == nil {
			return dest, nil
		}
		_ = os.Remove(dest)
		if errors.Is(err, ErrMissing) || ctx.Err() != nil {
			return "", err
		}
		lastErr = err // corrupt downloads are retried too: a truncated transfer looks the same
	}
	return "", fmt.Errorf("%s after %d attempts: %w", f.Name, c.attempts, lastErr)
}

func (c *Client) downloadOnce(ctx context.Context, f File, dest string) error {
	resp, err := c.do(ctx, f.Name, "")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	out, err := os.Create(dest)
	if err != nil {
		return err
	}
	h := md5.New()
	n, copyErr := io.Copy(io.MultiWriter(out, h), resp.Body)
	if err := out.Close(); err != nil && copyErr == nil {
		copyErr = err
	}
	if copyErr != nil {
		return copyErr
	}
	if n != f.Size {
		return fmt.Errorf("%s: %d bytes, list says %d: %w", f.Name, n, f.Size, ErrCorrupt)
	}
	if sum := hex.EncodeToString(h.Sum(nil)); sum != f.MD5 {
		return fmt.Errorf("%s: MD5 %s, list says %s: %w", f.Name, sum, f.MD5, ErrCorrupt)
	}
	return nil
}

// get reads a whole small file (or a range of it). whole is false when the server returned only the range asked for.
func (c *Client) get(ctx context.Context, name, rng string) (body []byte, whole bool, err error) {
	body, whole, _, err = c.getRange(ctx, name, rng)
	return body, whole, err
}

// getRange is get, also returning the file's total size (from Content-Range, or the body's length).
func (c *Client) getRange(ctx context.Context, name, rng string) (body []byte, whole bool, total int64, err error) {
	var lastErr error
	for attempt := range c.attempts {
		if attempt > 0 {
			if err := c.sleep(ctx, c.backoff<<(attempt-1)); err != nil {
				return nil, false, 0, err
			}
		}
		resp, err := c.do(ctx, name, rng)
		if err != nil {
			if errors.Is(err, ErrMissing) || ctx.Err() != nil {
				return nil, false, 0, err
			}
			lastErr = err
			continue
		}
		body, err = io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			lastErr = err
			continue
		}
		cr := resp.Header.Get("Content-Range")
		whole = resp.StatusCode == http.StatusOK || strings.HasPrefix(cr, "bytes 0-")
		total = int64(len(body))
		if i := strings.LastIndexByte(cr, '/'); resp.StatusCode == http.StatusPartialContent && i >= 0 {
			if n, err := strconv.ParseInt(cr[i+1:], 10, 64); err == nil {
				total = n
			}
		}
		return body, whole, total, nil
	}
	return nil, false, 0, fmt.Errorf("after %d attempts: %w", c.attempts, lastErr)
}

// do sends one GET. A 404 becomes ErrMissing; any other status but 200 and 206 is an error.
func (c *Client) do(ctx context.Context, name, rng string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+name, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", c.userAgent)
	if rng != "" {
		req.Header.Set("Range", rng)
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	switch resp.StatusCode {
	case http.StatusOK, http.StatusPartialContent:
		return resp, nil
	case http.StatusNotFound:
		resp.Body.Close()
		return nil, fmt.Errorf("%s: %w", name, ErrMissing)
	default:
		resp.Body.Close()
		return nil, fmt.Errorf("%s: HTTP %d", name, resp.StatusCode)
	}
}

// Unzip extracts the single file in a GDELT zip into dir and returns its path. A bad zip is ErrCorrupt.
func Unzip(zipPath, dir string) (string, error) {
	z, err := zip.OpenReader(zipPath)
	if err != nil {
		return "", fmt.Errorf("%s: %v: %w", filepath.Base(zipPath), err, ErrCorrupt)
	}
	defer z.Close()
	if len(z.File) != 1 {
		return "", fmt.Errorf("%s: %d files in the zip, expected 1: %w", filepath.Base(zipPath), len(z.File), ErrCorrupt)
	}
	zf := z.File[0]
	in, err := zf.Open()
	if err != nil {
		return "", fmt.Errorf("%s: %v: %w", filepath.Base(zipPath), err, ErrCorrupt)
	}
	defer in.Close()
	dest := filepath.Join(dir, strings.TrimSuffix(filepath.Base(zipPath), ".zip"))
	out, err := os.Create(dest)
	if err != nil {
		return "", err
	}
	if _, err := io.Copy(out, in); err != nil { // the zip's CRC check runs here
		out.Close()
		os.Remove(dest)
		return "", fmt.Errorf("%s: %v: %w", filepath.Base(zipPath), err, ErrCorrupt)
	}
	if err := out.Close(); err != nil {
		return "", err
	}
	return dest, nil
}

func sleepCtx(ctx context.Context, d time.Duration) error {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-t.C:
		return nil
	}
}
