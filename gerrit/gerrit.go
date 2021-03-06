// Copyright 2015 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

// Package gerrit contains code to interact with Gerrit servers.
package gerrit

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Client is a Gerrit client.
type Client struct {
	url  string // URL prefix, e.g. "https://go-review.googlesource.com/a" (without trailing slash)
	auth Auth

	// HTTPClient optionally specifies an HTTP client to use
	// instead of http.DefaultClient.
	HTTPClient *http.Client
}

// NewClient returns a new Gerrit client with the given URL prefix
// and authentication mode.
// The url should be just the scheme and hostname.
// If auth is nil, a default is used, or requests are made unauthenticated.
func NewClient(url string, auth Auth) *Client {
	if auth == nil {
		// TODO(bradfitz): use GitCookies auth, once that exists
		auth = NoAuth
	}
	return &Client{
		url:  strings.TrimSuffix(url, "/"),
		auth: auth,
	}
}

func (c *Client) httpClient() *http.Client {
	if c.HTTPClient != nil {
		return c.HTTPClient
	}
	return http.DefaultClient
}

func (c *Client) do(dst interface{}, method, path string, arg url.Values, body interface{}) error {
	var bodyr io.Reader
	var contentType string
	if body != nil {
		v, err := json.MarshalIndent(body, "", "  ")
		if err != nil {
			return err
		}
		bodyr = bytes.NewReader(v)
		contentType = "application/json"
	}
	// slashA is either "/a" (for authenticated requests) or "" for unauthenticated.
	// See https://gerrit-review.googlesource.com/Documentation/rest-api.html#authentication
	slashA := "/a"
	if _, ok := c.auth.(noAuth); ok {
		slashA = ""
	}
	var err error
	req, err := http.NewRequest(method, c.url+slashA+path+"?"+arg.Encode(), bodyr)
	if err != nil {
		return err
	}
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	c.auth.setAuth(c, req)
	res, err := c.httpClient().Do(req)
	if err != nil {
		return err
	}
	defer res.Body.Close()

	if res.StatusCode != http.StatusOK {
		body, _ := ioutil.ReadAll(io.LimitReader(res.Body, 4<<10))
		return fmt.Errorf("HTTP status %s; %s", res.Status, body)
	}

	// The JSON response begins with an XSRF-defeating header
	// like ")]}\n". Read that and skip it.
	br := bufio.NewReader(res.Body)
	if _, err := br.ReadSlice('\n'); err != nil {
		return err
	}
	return json.NewDecoder(br).Decode(dst)
}

// ChangeInfo is a Gerrit data structure.
// See https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#change-info
type ChangeInfo struct {
	// ID is the ID of the change in the format
	// "'<project>~<branch>~<Change-Id>'", where 'project',
	// 'branch' and 'Change-Id' are URL encoded. For 'branch' the
	// refs/heads/ prefix is omitted.
	ID           string `json:"id"`
	ChangeNumber int    `json:"_number"`

	Project string `json:"project"`

	// Branch is the name of the target branch.
	// The refs/heads/ prefix is omitted.
	Branch string `json:"branch"`

	ChangeID string `json:"change_id"`

	Subject string `json:"subject"`

	// Status is the status of the change (NEW, SUBMITTED, MERGED,
	// ABANDONED, DRAFT).
	Status string `json:"status"`

	Created  TimeStamp `json:"created"`
	Updated  TimeStamp `json:"updated"`
	Mergable bool      `json:"mergable"`

	// CurrentRevision is the commit ID of the current patch set
	// of this change.  This is only set if the current revision
	// is requested or if all revisions are requested (fields
	// "CURRENT_REVISION" or "ALL_REVISIONS").
	CurrentRevision string `json:"current_revision"`

	// Revisions maps a commit ID of the patch set to a
	// RevisionInfo entity.
	//
	// Only set if the current revision is requested (in which
	// case it will only contain a key for the current revision)
	// or if all revisions are requested.
	Revisions map[string]RevisionInfo `json:"revisions"`

	// Owner is the author of the change.
	// The details are only filled in if field "DETAILED_ACCOUNTS" is requested.
	Owner *AccountInfo `json:"owner"`

	// Messages are included if field "MESSAGES" is requested.
	Messages []ChangeMessageInfo `json:"messages"`

	Labels map[string]LabelInfo `json:"labels"`

	// TODO: more as needed

	// MoreChanges is set on the last change from QueryChanges if
	// the result set is truncated by an 'n' parameter.
	MoreChanges bool `json:"_more_changes"`
}

type AccountInfo struct {
	NumericID int64  `json:"_account_id"`
	Name      string `json:"name,omitempty"`
	Email     string `json:"email,omitempty"`
	Username  string `json:"username,omitempty"`
}

func (ai *AccountInfo) Equal(v *AccountInfo) bool {
	if ai == nil || v == nil {
		return false
	}
	return ai.NumericID == v.NumericID
}

type ChangeMessageInfo struct {
	ID             string       `json:"id"`
	Author         *AccountInfo `json:"author"`
	Time           TimeStamp    `json:"date"`
	Message        string       `json:"message"`
	RevisionNumber int          `json:"_revision_number"`
}

// The LabelInfo entity contains information about a label on a
// change, always corresponding to the current patch set.
//
// There are two options that control the contents of LabelInfo:
// LABELS and DETAILED_LABELS.
//
// For a quick summary of the state of labels, use LABELS.
//
// For detailed information about labels, including exact numeric
// votes for all users and the allowed range of votes for the current
// user, use DETAILED_LABELS.
type LabelInfo struct {
	// Optional means the label may be set, but it’s neither
	// necessary for submission nor does it block submission if
	// set.
	Optional bool `json:"optional"`

	// Fields set by LABELS field option:

	All []ApprovalInfo `json:"all"`
}

type ApprovalInfo struct {
	AccountInfo
	Value int       `json:"value"`
	Date  TimeStamp `json:"date"`
}

// The RevisionInfo entity contains information about a patch set. Not
// all fields are returned by default. Additional fields can be
// obtained by adding o parameters as described at:
// https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#list-changes
type RevisionInfo struct {
	Commit *CommitInfo `json:"commit"`

	// TODO: more
}

type CommitInfo struct {
	Author    GitPersonInfo `json:"author"`
	Committer GitPersonInfo `json:"committer"`
	CommitID  string        `json:"commit"`
	Subject   string        `json:"subject"`
	Message   string        `json:"message"`
}

type GitPersonInfo struct {
	Name     string    `json:"name"`
	Email    string    `json:"Email"`
	Date     TimeStamp `json:"date"`
	TZOffset int       `json:"tz"`
}

// QueryChangesOpt are options for QueryChanges.
type QueryChangesOpt struct {
	// N is the number of results to return.
	// If 0, the 'n' parameter is not sent to Gerrit.
	N int

	// Fields are optional fields to also return.
	// Example strings include "ALL_REVISIONS", "LABELS", "MESSAGES".
	// For a complete list, see:
	// https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#change-info
	Fields []string
}

func condInt(n int) []string {
	if n != 0 {
		return []string{strconv.Itoa(n)}
	}
	return nil
}

// QueryChanges queries changes. The q parameter is a Gerrit search query.
// For the API call, see https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#list-changes
// For the query syntax, see https://gerrit-review.googlesource.com/Documentation/user-search.html#_search_operators
func (c *Client) QueryChanges(q string, opts ...QueryChangesOpt) ([]*ChangeInfo, error) {
	var opt QueryChangesOpt
	switch len(opts) {
	case 0:
	case 1:
		opt = opts[0]
	default:
		return nil, errors.New("only 1 option struct supported")
	}
	var changes []*ChangeInfo
	err := c.do(&changes, "GET", "/changes/", url.Values{
		"q": {q},
		"n": condInt(opt.N),
		"o": opt.Fields,
	}, nil)
	return changes, err
}

type ReviewInput struct {
	Message string         `json:"message,omitempty"`
	Labels  map[string]int `json:"labels,omitempty"`
}

type reviewInfo struct {
	Labels map[string]int `json:"labels,omitempty"`
}

// SetReview leaves a message on a change and/or modifies labels.
// For the API call, see https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#set-review
// The changeID is https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#change-id
// The revision is https://gerrit-review.googlesource.com/Documentation/rest-api-changes.html#revision-id
func (c *Client) SetReview(changeID, revision string, review ReviewInput) error {
	var res reviewInfo
	return c.do(&res, "POST", fmt.Sprintf("/changes/%s/revisions/%s/review", changeID, revision),
		nil, review)
}

type TimeStamp time.Time

// Gerrit's timestamp layout is like time.RFC3339Nano, but with a space instead of the "T",
// and without a timezone (it's always in UTC).
const timeStampLayout = "2006-01-02 15:04:05.999999999"

func (ts *TimeStamp) UnmarshalJSON(p []byte) error {
	if len(p) < 2 {
		return errors.New("Timestamp too short")
	}
	if p[0] != '"' || p[len(p)-1] != '"' {
		return errors.New("not double-quoted")
	}
	s := strings.Trim(string(p), "\"")
	t, err := time.Parse(timeStampLayout, s)
	if err != nil {
		return err
	}
	*ts = TimeStamp(t)
	return nil
}

func (ts TimeStamp) Time() time.Time { return time.Time(ts) }
