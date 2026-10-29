// Package loki pushes audit records to Loki (SPEC.md §8.1) and spools them
// to disk when Loki is unreachable.
package loki

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	Labels map[string]string `json:"labels"`
	Time   time.Time         `json:"time"`
	Line   string            `json:"line"`
}

// Pusher sends entries to Loki. Implementations must be all-or-nothing
// from the caller's point of view: an error means "assume nothing landed".
type Pusher interface {
	Push(ctx context.Context, entries []Entry) error
}

type Client struct {
	url, user, password, userAgent string
	http                           *http.Client
}

func New(baseURL, user, password, userAgent string) *Client {
	return &Client{
		url:       strings.TrimRight(baseURL, "/") + "/loki/api/v1/push",
		user:      user,
		password:  password,
		userAgent: userAgent,
		http:      &http.Client{Timeout: 15 * time.Second},
	}
}

func labelKey(l map[string]string) string {
	keys := make([]string, 0, len(l))
	for k := range l {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		b.WriteString(k + "=" + strconv.Quote(l[k]) + ",")
	}
	return b.String()
}

type stream struct {
	Stream map[string]string `json:"stream"`
	Values [][2]string       `json:"values"`
}

// Body builds the JSON push payload, one stream per distinct label set.
func Body(entries []Entry) ([]byte, error) {
	var order []string
	byKey := map[string]*stream{}
	for _, e := range entries {
		k := labelKey(e.Labels)
		s, ok := byKey[k]
		if !ok {
			s = &stream{Stream: e.Labels}
			byKey[k] = s
			order = append(order, k)
		}
		s.Values = append(s.Values, [2]string{strconv.FormatInt(e.Time.UnixNano(), 10), e.Line})
	}
	payload := struct {
		Streams []*stream `json:"streams"`
	}{}
	for _, k := range order {
		payload.Streams = append(payload.Streams, byKey[k])
	}
	return json.Marshal(payload)
}

func (c *Client) Push(ctx context.Context, entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	body, err := Body(entries)
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	req.SetBasicAuth(c.user, c.password)
	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("loki: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return fmt.Errorf("loki: HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(b)))
	}
	return nil
}

// Spool keeps entries that could not be pushed, one file per batch, and
// replays them in order.
type Spool struct {
	dir string
	mu  sync.Mutex
	seq int
}

func NewSpool(dir string) (*Spool, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	return &Spool{dir: dir}, nil
}

func (s *Spool) Add(entries []Entry) error {
	if len(entries) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.seq++
	b, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	name := fmt.Sprintf("%020d-%06d.json", time.Now().UnixNano(), s.seq)
	tmp := filepath.Join(s.dir, "."+name)
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	if _, err := f.Write(b); err != nil {
		f.Close()
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		return err
	}
	if err := f.Close(); err != nil {
		return err
	}
	return os.Rename(tmp, filepath.Join(s.dir, name))
}

// Pending returns the number of spooled batches.
func (s *Spool) Pending() int {
	files, _ := filepath.Glob(filepath.Join(s.dir, "[0-9]*.json"))
	return len(files)
}

// Flush pushes spooled batches oldest first and stops at the first error,
// keeping order.
func (s *Spool) Flush(ctx context.Context, p Pusher) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	files, err := filepath.Glob(filepath.Join(s.dir, "[0-9]*.json"))
	if err != nil {
		return 0, err
	}
	sort.Strings(files)
	sent := 0
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			return sent, err
		}
		var entries []Entry
		if err := json.Unmarshal(b, &entries); err != nil {
			// Never drop audit data silently: keep the file for a human.
			bad := f + ".corrupt"
			_ = os.Rename(f, bad)
			return sent, fmt.Errorf("spool file %s is corrupt (kept as %s): %w", f, bad, err)
		}
		if err := p.Push(ctx, entries); err != nil {
			return sent, err
		}
		if err := os.Remove(f); err != nil {
			return sent, err
		}
		sent++
	}
	return sent, nil
}

// Stream buffers output lines for one job and pushes them in batches,
// spooling a batch that cannot be pushed. Lines are never dropped.
type Stream struct {
	p      Pusher
	spool  *Spool
	labels map[string]string

	mu  sync.Mutex
	buf []Entry
}

// MaxLine caps one output line; Loki's default limit is 256 KB.
const MaxLine = 64 << 10

func NewStream(p Pusher, spool *Spool, labels map[string]string) *Stream {
	return &Stream{p: p, spool: spool, labels: labels}
}

func (s *Stream) Add(t time.Time, line string) {
	if len(line) > MaxLine {
		line = line[:MaxLine] + " [peon: line truncated]"
	}
	s.mu.Lock()
	s.buf = append(s.buf, Entry{Labels: s.labels, Time: t, Line: line})
	s.mu.Unlock()
}

// Flush pushes buffered lines. If the push fails they go to the spool; an
// error is returned only if spooling fails too.
func (s *Stream) Flush(ctx context.Context) error {
	s.mu.Lock()
	batch := s.buf
	s.buf = nil
	s.mu.Unlock()
	if len(batch) == 0 {
		return nil
	}
	if s.spool != nil && s.spool.Pending() > 0 {
		// Keep ordering: earlier lines are waiting in the spool.
		return s.spool.Add(batch)
	}
	if err := s.p.Push(ctx, batch); err != nil {
		if s.spool == nil {
			return err
		}
		return s.spool.Add(batch)
	}
	return nil
}
