package http

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"
)

type Client struct {
	baseURL string
	client  *http.Client
	timeout time.Duration
}

func NewClient() *Client {
	return &Client{
		baseURL: "",
		client:  &http.Client{},
		timeout: 30 * time.Second,
	}
}

func (c *Client) SetBaseURL(url string) *Client {
	c.baseURL = url
	return c
}

func (c *Client) SetTimeout(timeout time.Duration) *Client {
	c.timeout = timeout
	c.client.Timeout = timeout
	return c
}

func (c *Client) GetTimeout() time.Duration {
	return c.timeout
}

func (c *Client) Post(endpoint string, body interface{}) (*http.Response, error) {
	var reqBody io.Reader
	if body != nil {
		jsonData, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("marshal request body: %w", err)
		}
		reqBody = bytes.NewReader(jsonData)
	}

	req, err := http.NewRequest("POST", c.baseURL+endpoint, reqBody)
	if err != nil {
		return nil, fmt.Errorf("create request: %w", err)
	}

	req.Header.Set("Content-Type", "application/json")

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("do request: %w", err)
	}
	return resp, nil
}
