package natsbus

import (
	"encoding/json"
	"log"
	"strings"

	natsgo "github.com/nats-io/nats.go"
)

// isValidSubjectPart checks that a string is safe to use as a NATS subject
// token. Dots, wildcards, and whitespace could allow subscription hijacking.
func isValidSubjectPart(s string) bool {
	return s != "" && !strings.ContainsAny(s, ".*> \t\n\r")
}

type Client struct {
	conn *natsgo.Conn
}

func New(url string) *Client {
	if url == "" {
		return &Client{}
	}
	conn, err := natsgo.Connect(url)
	if err != nil {
		log.Printf("nats: failed to connect to %s: %v (continuing without NATS)", url, err)
		return &Client{}
	}
	log.Printf("nats: connected to %s", url)
	return &Client{conn: conn}
}

func (c *Client) Publish(teamID, event string, data any) {
	if c.conn == nil {
		return
	}
	if !isValidSubjectPart(teamID) {
		log.Printf("nats: refusing to publish — invalid teamID in subject: %q", teamID)
		return
	}
	subject := "asynkor.team." + teamID + "." + event
	payload, err := json.Marshal(data)
	if err != nil {
		return
	}
	_ = c.conn.Publish(subject, payload)
}

func (c *Client) Subscribe(subject string, handler func(subject string, data []byte)) (func(), error) {
	if c.conn == nil {
		return func() {}, nil
	}
	sub, err := c.conn.Subscribe(subject, func(msg *natsgo.Msg) {
		handler(msg.Subject, msg.Data)
	})
	if err != nil {
		return nil, err
	}
	return func() { _ = sub.Unsubscribe() }, nil
}

func (c *Client) Close() {
	if c.conn != nil {
		c.conn.Close()
	}
}
