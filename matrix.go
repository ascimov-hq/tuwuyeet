package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
)

type MatrixError struct {
	Status  int
	Code    string `json:"errcode"`
	Message string `json:"error"`
}

func (e *MatrixError) Error() string {
	return fmt.Sprintf("%d %s: %s", e.Status, e.Code, e.Message)
}

var ErrNotAdmin = errors.New("bot is not joined to the admin room; restart tuwunel so admin_execute runs make-user-admin")

type Client struct {
	base      string
	token     string
	localpart string
	userID    string
	http      *http.Client
	txn       atomic.Uint64
}

func NewClient(homeserver, serverName string, reg Registration) *Client {
	return &Client{
		base:      homeserver,
		token:     reg.ASToken,
		localpart: reg.SenderLocalpart,
		userID:    "@" + reg.SenderLocalpart + ":" + serverName,
		http:      &http.Client{Timeout: 60 * time.Second},
	}
}

func (c *Client) do(ctx context.Context, method, path string, query url.Values, body, out any) error {
	u := c.base + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	var rd io.Reader
	if body != nil {
		raw, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rd = bytes.NewReader(raw)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rd)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode >= 300 {
		me := &MatrixError{Status: resp.StatusCode}
		_ = json.Unmarshal(raw, me)
		return me
	}
	if out != nil {
		return json.Unmarshal(raw, out)
	}
	return nil
}

func (c *Client) EnsureRegistered(ctx context.Context) error {
	body := map[string]any{
		"type":          "m.login.application_service",
		"username":      c.localpart,
		"inhibit_login": true,
	}
	err := c.do(ctx, http.MethodPost, "/_matrix/client/v3/register", nil, body, nil)
	var me *MatrixError
	if errors.As(err, &me) && me.Code == "M_USER_IN_USE" {
		return nil
	}
	return err
}

func (c *Client) ResolveAlias(ctx context.Context, alias string) (string, error) {
	var out struct {
		RoomID string `json:"room_id"`
	}
	err := c.do(ctx, http.MethodGet, "/_matrix/client/v3/directory/room/"+url.PathEscape(alias), nil, nil, &out)
	return out.RoomID, err
}

func (c *Client) IsJoined(ctx context.Context, roomID string) (bool, error) {
	var out struct {
		JoinedRooms []string `json:"joined_rooms"`
	}
	if err := c.do(ctx, http.MethodGet, "/_matrix/client/v3/joined_rooms", nil, nil, &out); err != nil {
		return false, err
	}
	return slices.Contains(out.JoinedRooms, roomID), nil
}

func (c *Client) SendText(ctx context.Context, roomID, text string) (string, error) {
	txn := strconv.FormatInt(time.Now().UnixNano(), 36) + "." + strconv.FormatUint(c.txn.Add(1), 10)
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/send/m.room.message/" + txn
	var out struct {
		EventID string `json:"event_id"`
	}
	err := c.do(ctx, http.MethodPut, path, nil, map[string]string{"msgtype": "m.text", "body": text}, &out)
	return out.EventID, err
}

type roomEvent struct {
	Type    string `json:"type"`
	Sender  string `json:"sender"`
	Content struct {
		Body      string `json:"body"`
		RelatesTo struct {
			InReplyTo struct {
				EventID string `json:"event_id"`
			} `json:"m.in_reply_to"`
		} `json:"m.relates_to"`
	} `json:"content"`
}

func (c *Client) findReply(ctx context.Context, roomID, eventID string) (string, bool, error) {
	var out struct {
		Chunk []roomEvent `json:"chunk"`
	}
	q := url.Values{"dir": {"b"}, "limit": {"100"}}
	path := "/_matrix/client/v3/rooms/" + url.PathEscape(roomID) + "/messages"
	if err := c.do(ctx, http.MethodGet, path, q, nil, &out); err != nil {
		return "", false, err
	}
	for _, ev := range out.Chunk {
		if ev.Type == "m.room.message" && ev.Sender != c.userID && ev.Content.RelatesTo.InReplyTo.EventID == eventID {
			return ev.Content.Body, true, nil
		}
	}
	return "", false, nil
}

func (c *Client) WaitReply(ctx context.Context, roomID, eventID string) (string, error) {
	tick := time.NewTicker(3 * time.Second)
	defer tick.Stop()
	for {
		body, ok, err := c.findReply(ctx, roomID, eventID)
		if err == nil && ok {
			return body, nil
		}
		select {
		case <-ctx.Done():
			if err != nil {
				return "", fmt.Errorf("no reply to %s: %w (last poll error: %v)", eventID, ctx.Err(), err)
			}
			return "", fmt.Errorf("no reply to %s: %w", eventID, ctx.Err())
		case <-tick.C:
		}
	}
}

type Outcome struct {
	OK    bool
	Count int
	Body  string
}

var countPattern = regexp.MustCompile(`(?:Successfully deleted|Deleted) ([0-9]+) (?:rooms|total files)`)

func classifyReply(body string) Outcome {
	o := Outcome{Body: body}
	if strings.Contains(body, "Did not found any eligible MXCs") {
		o.OK = true
		return o
	}
	trimmed := strings.TrimSpace(body)
	switch {
	case strings.Contains(body, "Command failed with error"),
		strings.Contains(body, "Panic occurred"),
		strings.HasPrefix(trimmed, "error:"),
		strings.HasPrefix(trimmed, "Failed to build"):
		return o
	}
	o.OK = true
	if m := countPattern.FindStringSubmatch(body); m != nil {
		o.Count, _ = strconv.Atoi(m[1])
	}
	return o
}

func (c *Client) RunAdmin(ctx context.Context, serverName, command string, timeout time.Duration) (Outcome, error) {
	roomID, err := c.ResolveAlias(ctx, "#admins:"+serverName)
	if err != nil {
		return Outcome{}, fmt.Errorf("resolve admin room: %w", err)
	}
	joined, err := c.IsJoined(ctx, roomID)
	if err != nil {
		return Outcome{}, fmt.Errorf("check admin room membership: %w", err)
	}
	if !joined {
		return Outcome{}, ErrNotAdmin
	}
	eventID, err := c.SendText(ctx, roomID, command)
	if err != nil {
		return Outcome{}, fmt.Errorf("send command: %w", err)
	}
	wctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	body, err := c.WaitReply(wctx, roomID, eventID)
	if err != nil {
		return Outcome{}, err
	}
	return classifyReply(body), nil
}
