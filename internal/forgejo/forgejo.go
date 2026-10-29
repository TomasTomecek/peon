// Package forgejo is a minimal Forgejo REST client covering what peon
// needs: reading issue comments and their reactions, and posting comments.
package forgejo

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

type User struct {
	ID    int64  `json:"id"`
	Login string `json:"login"`
}

type Comment struct {
	ID        int64     `json:"id"`
	Body      string    `json:"body"`
	User      User      `json:"user"`
	HTMLURL   string    `json:"html_url"`
	IssueURL  string    `json:"issue_url"`
	PRURL     string    `json:"pull_request_url"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// IssueIndex returns the number of the issue or pull request the comment
// belongs to, taken from the last segment of its issue or PR URL.
func (c Comment) IssueIndex() (int64, error) {
	u := c.IssueURL
	if u == "" {
		u = c.PRURL
	}
	if u == "" {
		return 0, errors.New("comment has no issue or pull request URL")
	}
	n, err := strconv.ParseInt(path.Base(strings.TrimRight(u, "/")), 10, 64)
	if err != nil || n <= 0 {
		return 0, fmt.Errorf("cannot parse issue number from %q", u)
	}
	return n, nil
}

type Reaction struct {
	User      User      `json:"user"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

// HTTPError is a non-2xx response.
type HTTPError struct {
	Method, URL string
	Status      int
	Body        string
}

func (e *HTTPError) Error() string {
	return fmt.Sprintf("forgejo: %s %s: HTTP %d: %s", e.Method, e.URL, e.Status, e.Body)
}

// IsNotFound reports whether err is an HTTP 404 from Forgejo.
func IsNotFound(err error) bool {
	var he *HTTPError
	return errors.As(err, &he) && he.Status == http.StatusNotFound
}

type Client struct {
	base      *url.URL
	token     string
	http      *http.Client
	userAgent string
}

// New creates a client for the Forgejo instance at baseURL (without
// /api/v1).
func New(baseURL, token, userAgent string) (*Client, error) {
	u, err := url.Parse(strings.TrimRight(baseURL, "/"))
	if err != nil {
		return nil, err
	}
	return &Client{
		base:      u,
		token:     token,
		http:      &http.Client{Timeout: 30 * time.Second},
		userAgent: userAgent,
	}, nil
}

const pageSize = 50

func splitRepo(repo string) (string, string) {
	owner, name, _ := strings.Cut(repo, "/")
	return url.PathEscape(owner), url.PathEscape(name)
}

func (c *Client) do(ctx context.Context, method, p string, q url.Values, in, out any) error {
	u := *c.base
	u.Path = c.base.Path + "/api/v1" + p
	u.RawPath = ""
	if q != nil {
		u.RawQuery = q.Encode()
	}
	var body io.Reader
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}
		body = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), body)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "token "+c.token)
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", c.userAgent)
	if in != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return &HTTPError{Method: method, URL: u.Redacted(), Status: resp.StatusCode, Body: strings.TrimSpace(string(b))}
	}
	if out == nil {
		return nil
	}
	return json.NewDecoder(io.LimitReader(resp.Body, 16<<20)).Decode(out)
}

// ListRepoComments returns all issue and PR comments in repo updated at or
// after since, following pagination.
func (c *Client) ListRepoComments(ctx context.Context, repo string, since time.Time) ([]Comment, error) {
	owner, name := splitRepo(repo)
	var all []Comment
	for page := 1; ; page++ {
		q := url.Values{
			"since": {since.UTC().Format(time.RFC3339)},
			"page":  {strconv.Itoa(page)},
			"limit": {strconv.Itoa(pageSize)},
		}
		var batch []Comment
		if err := c.do(ctx, http.MethodGet, "/repos/"+owner+"/"+name+"/issues/comments", q, nil, &batch); err != nil {
			return nil, err
		}
		all = append(all, batch...)
		if len(batch) < pageSize {
			return all, nil
		}
		if page >= 200 {
			return nil, errors.New("forgejo: more than 200 pages of comments; refusing to continue")
		}
	}
}

func (c *Client) GetComment(ctx context.Context, repo string, id int64) (Comment, error) {
	owner, name := splitRepo(repo)
	var out Comment
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/comments/%d", owner, name, id), nil, nil, &out)
	return out, err
}

func (c *Client) ListCommentReactions(ctx context.Context, repo string, id int64) ([]Reaction, error) {
	owner, name := splitRepo(repo)
	var out []Reaction
	err := c.do(ctx, http.MethodGet, fmt.Sprintf("/repos/%s/%s/issues/comments/%d/reactions", owner, name, id), nil, nil, &out)
	return out, err
}

func (c *Client) CreateComment(ctx context.Context, repo string, issue int64, body string) (Comment, error) {
	owner, name := splitRepo(repo)
	var out Comment
	err := c.do(ctx, http.MethodPost, fmt.Sprintf("/repos/%s/%s/issues/%d/comments", owner, name, issue), nil,
		map[string]string{"body": body}, &out)
	return out, err
}
