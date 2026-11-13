// Package kafbat reads the QC Kafka through a Kafbat UI (kafka-ui) instance:
// clusters, topics, messages and consumer groups. It only reads: there is no
// produce, delete, offset reset or topic change here.
//
// Kafbat behind OAuth2 SSO keeps the login in a Spring session cookie
// (SESSION); the client sends the operator's Cookie header and treats a
// redirect to the login page as an expired session.
package kafbat

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strconv"
	"strings"
	"time"
)

// SecretCookie is where the operator's Kafbat Cookie header is stored.
const SecretCookie = "kafbat/cookie"

// SessionCookieName is the cookie that carries the Kafbat login.
const SessionCookieName = "SESSION"

var (
	ErrNoCredentials = errors.New("chưa đăng nhập Kafka UI QC: `taskpilot auth kafka` (tab Status, phím K)")
	ErrUnauthorized  = errors.New("Kafka UI từ chối phiên đăng nhập (hết hạn): đăng nhập lại bằng `taskpilot auth kafka`")
	ErrForbidden     = errors.New("Kafka UI: tài khoản không có quyền với tài nguyên này")
	ErrNotFound      = errors.New("Kafka UI: không có cluster/topic/consumer group này")
)

// Client talks to one Kafbat UI.
type Client struct {
	BaseURL string
	Cookie  string // Cookie header value (SESSION=…)
	HTTP    *http.Client
}

func (c *Client) http() *http.Client {
	hc := &http.Client{Timeout: 60 * time.Second}
	if c.HTTP != nil {
		cp := *c.HTTP
		hc = &cp
	}
	// A redirect means "log in first" (to /oauth2/authorization/…): never follow it.
	hc.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return hc
}

func (c *Client) get(ctx context.Context, p string, q url.Values, accept string) (*http.Response, error) {
	if c.Cookie == "" {
		return nil, ErrNoCredentials
	}
	u := strings.TrimRight(c.BaseURL, "/") + p
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Cookie", c.Cookie)
	req.Header.Set("Accept", accept)
	resp, err := c.http().Do(req)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.StatusCode == http.StatusOK:
		return resp, nil
	case resp.StatusCode >= 300 && resp.StatusCode < 400, resp.StatusCode == http.StatusUnauthorized:
		resp.Body.Close()
		return nil, ErrUnauthorized
	case resp.StatusCode == http.StatusForbidden:
		resp.Body.Close()
		return nil, ErrForbidden
	case resp.StatusCode == http.StatusNotFound:
		resp.Body.Close()
		return nil, ErrNotFound
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return nil, fmt.Errorf("kafka-ui GET %s: HTTP %d: %s", p, resp.StatusCode, strings.TrimSpace(string(body[:min(len(body), 300)])))
}

func (c *Client) getJSON(ctx context.Context, p string, q url.Values, out any) error {
	resp, err := c.get(ctx, p, q, "application/json")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return err
	}
	if !json.Valid(body) {
		// The login page (HTML) instead of JSON: the session is gone.
		return ErrUnauthorized
	}
	return json.Unmarshal(body, out)
}

func seg(s string) string { return url.PathEscape(s) }

// Cluster is one Kafka cluster known to the UI.
type Cluster struct {
	Name                 string `json:"name"`
	Status               string `json:"status"` // ONLINE, OFFLINE
	BrokerCount          int    `json:"brokerCount"`
	TopicCount           int    `json:"topicCount"`
	OnlinePartitionCount int    `json:"onlinePartitionCount"`
	Version              string `json:"version"`
	ReadOnly             bool   `json:"readOnly"`
}

// Clusters lists the clusters; it is also the session check.
func (c *Client) Clusters(ctx context.Context) ([]Cluster, error) {
	var out []Cluster
	err := c.getJSON(ctx, "/api/clusters", nil, &out)
	return out, err
}

// User is the logged-in account, when the UI tells it.
func (c *Client) User(ctx context.Context) (string, error) {
	var out struct {
		UserInfo *struct {
			Username string `json:"username"`
		} `json:"userInfo"`
	}
	if err := c.getJSON(ctx, "/api/authorization", nil, &out); err != nil {
		return "", err
	}
	if out.UserInfo == nil {
		return "", nil
	}
	return out.UserInfo.Username, nil
}

// Topic is a topic summary.
type Topic struct {
	Name              string `json:"name"`
	Internal          bool   `json:"internal"`
	PartitionCount    int    `json:"partitionCount"`
	ReplicationFactor int    `json:"replicationFactor"`
	SegmentSize       int64  `json:"segmentSize"`
	CleanUpPolicy     string `json:"cleanUpPolicy"`
}

// Topics lists the non-internal topics whose name contains search (all pages).
func (c *Client) Topics(ctx context.Context, cluster, search string) ([]Topic, error) {
	var all []Topic
	for page := 1; page <= 100; page++ {
		q := url.Values{"page": {strconv.Itoa(page)}, "perPage": {"200"}, "showInternal": {"false"}, "orderBy": {"NAME"}}
		if search != "" {
			q.Set("search", search)
		}
		var out struct {
			PageCount int     `json:"pageCount"`
			Topics    []Topic `json:"topics"`
		}
		if err := c.getJSON(ctx, "/api/clusters/"+seg(cluster)+"/topics", q, &out); err != nil {
			return nil, err
		}
		all = append(all, out.Topics...)
		if page >= out.PageCount || len(out.Topics) == 0 {
			break
		}
	}
	return all, nil
}

// Partition is one partition of a topic with its offsets.
type Partition struct {
	Partition int   `json:"partition"`
	OffsetMin int64 `json:"offsetMin"`
	OffsetMax int64 `json:"offsetMax"`
}

// TopicDetail is a topic with its partitions.
type TopicDetail struct {
	Topic
	Partitions []Partition `json:"partitions"`
}

// Topic returns one topic with its partition offsets.
func (c *Client) Topic(ctx context.Context, cluster, topic string) (*TopicDetail, error) {
	var out TopicDetail
	if err := c.getJSON(ctx, "/api/clusters/"+seg(cluster)+"/topics/"+seg(topic), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Group is a consumer group summary.
type Group struct {
	GroupID     string `json:"groupId"`
	Members     int    `json:"members"`
	Topics      int    `json:"topics"`
	State       string `json:"state"`
	ConsumerLag *int64 `json:"consumerLag"`
	// MessagesBehind is the older name of ConsumerLag.
	MessagesBehind *int64           `json:"messagesBehind"`
	Partitions     []GroupPartition `json:"partitions,omitempty"`
}

// Lag is the group's total lag (-1 when unknown).
func (g Group) Lag() int64 {
	switch {
	case g.ConsumerLag != nil:
		return *g.ConsumerLag
	case g.MessagesBehind != nil:
		return *g.MessagesBehind
	}
	return -1
}

// GroupPartition is a consumer group's position on one partition.
type GroupPartition struct {
	Topic         string `json:"topic"`
	Partition     int    `json:"partition"`
	CurrentOffset *int64 `json:"currentOffset"`
	EndOffset     *int64 `json:"endOffset"`
	ConsumerLag   *int64 `json:"consumerLag"`
	ConsumerID    string `json:"consumerId"`
	Host          string `json:"host"`
}

// TopicGroups lists the consumer groups reading a topic.
func (c *Client) TopicGroups(ctx context.Context, cluster, topic string) ([]Group, error) {
	var out []Group
	err := c.getJSON(ctx, "/api/clusters/"+seg(cluster)+"/topics/"+seg(topic)+"/consumer-groups", nil, &out)
	return out, err
}

// Group returns one consumer group with its partitions.
func (c *Client) Group(ctx context.Context, cluster, id string) (*Group, error) {
	var out Group
	if err := c.getJSON(ctx, "/api/clusters/"+seg(cluster)+"/consumer-groups/"+seg(id), nil, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// Message is one Kafka record as the UI renders it.
type Message struct {
	Partition int       `json:"partition"`
	Offset    int64     `json:"offset"`
	Timestamp time.Time `json:"timestamp"`
	Key       *string   `json:"key"`
	Value     *string   `json:"value"`
	// Content is the value in Kafbat before 1.5.
	Content *string           `json:"content"`
	Headers map[string]string `json:"headers"`
}

// Body is the message value as the UI deserialized it (nil: a null value).
func (m Message) Body() *string {
	if m.Value != nil {
		return m.Value
	}
	return m.Content
}

// MessagesQuery selects messages of a topic.
type MessagesQuery struct {
	// Mode: LATEST (default), EARLIEST, FROM_OFFSET, TO_OFFSET, FROM_TIMESTAMP, TO_TIMESTAMP.
	Mode       string
	Partitions []int
	Offset     *int64     // FROM_OFFSET / TO_OFFSET
	Since      *time.Time // FROM_TIMESTAMP / TO_TIMESTAMP
	Filter     string     // substring of key or value (server side)
	Limit      int
}

// Messages reads up to q.Limit messages (newest first for LATEST). Kafbat
// streams them as server-sent events; tailing is not supported here.
func (c *Client) Messages(ctx context.Context, cluster, topic string, q MessagesQuery) ([]Message, error) {
	mode := strings.ToUpper(q.Mode)
	if mode == "" {
		mode = "LATEST"
	}
	if mode == "TAILING" {
		return nil, errors.New("kafka-ui: TAILING is not supported (it never ends)")
	}
	if q.Limit <= 0 {
		q.Limit = 20
	}
	// No keySerde/valueSerde: the UI picks each topic's preferred one (String,
	// or Avro/Protobuf through the schema registry).
	v := url.Values{"mode": {mode}, "limit": {strconv.Itoa(q.Limit)}}
	for _, p := range q.Partitions {
		v.Add("partitions", strconv.Itoa(p))
	}
	if q.Offset != nil {
		v.Set("offset", strconv.FormatInt(*q.Offset, 10))
	}
	if q.Since != nil {
		v.Set("timestamp", strconv.FormatInt(q.Since.UnixMilli(), 10))
	}
	if q.Filter != "" {
		v.Set("stringFilter", q.Filter)
	}
	resp, err := c.get(ctx, "/api/clusters/"+seg(cluster)+"/topics/"+seg(topic)+"/messages/v2", v, "text/event-stream")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	return readEvents(resp.Body, q.Limit)
}

// readEvents parses Kafbat's TopicMessageEvent stream.
func readEvents(r io.Reader, limit int) ([]Message, error) {
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 16<<20)
	var out []Message
	var data strings.Builder
	flush := func() (bool, error) {
		defer data.Reset()
		if data.Len() == 0 {
			return false, nil
		}
		var ev struct {
			Type    string   `json:"type"`
			Message *Message `json:"message"`
			Phase   *struct {
				Name string `json:"name"`
			} `json:"phase"`
		}
		if err := json.Unmarshal([]byte(data.String()), &ev); err != nil {
			return false, nil // not an event we know
		}
		switch ev.Type {
		case "MESSAGE":
			if ev.Message != nil {
				out = append(out, *ev.Message)
			}
		case "DONE":
			return true, nil
		case "ERROR":
			msg := "unknown error"
			if ev.Phase != nil && ev.Phase.Name != "" {
				msg = ev.Phase.Name
			}
			return true, fmt.Errorf("kafka-ui: %s", msg)
		}
		return len(out) >= limit, nil
	}
	for sc.Scan() {
		line := sc.Text()
		if line == "" {
			if done, err := flush(); done || err != nil {
				return out, err
			}
			continue
		}
		if d, ok := strings.CutPrefix(line, "data:"); ok {
			data.WriteString(strings.TrimPrefix(d, " "))
		}
	}
	if _, err := flush(); err != nil {
		return out, err
	}
	return out, sc.Err()
}

// Allowed reports whether topic matches one of the patterns: a glob
// (payment-*, *.order.*) or an exact name.
func Allowed(topic string, patterns []string) bool {
	for _, p := range patterns {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		if p == topic {
			return true
		}
		if ok, err := path.Match(p, topic); err == nil && ok {
			return true
		}
	}
	return false
}

// FilterTopics keeps the topics a project may read.
func FilterTopics(ts []Topic, patterns []string) []Topic {
	var out []Topic
	for _, t := range ts {
		if Allowed(t.Name, patterns) {
			out = append(out, t)
		}
	}
	return out
}

// FormatTopics renders topics one per line.
func FormatTopics(ts []Topic) string {
	var b strings.Builder
	for _, t := range ts {
		fmt.Fprintf(&b, "%s\tpartitions=%d rf=%d\n", t.Name, t.PartitionCount, t.ReplicationFactor)
	}
	return b.String()
}

// FormatTopic renders a topic's partition offsets.
func FormatTopic(t *TopicDetail) string {
	var b strings.Builder
	var total int64
	ps := append([]Partition(nil), t.Partitions...)
	sort.Slice(ps, func(i, j int) bool { return ps[i].Partition < ps[j].Partition })
	for _, p := range ps {
		total += p.OffsetMax - p.OffsetMin
		fmt.Fprintf(&b, "  partition %d: offsets %d..%d\n", p.Partition, p.OffsetMin, p.OffsetMax)
	}
	return fmt.Sprintf("%s: %d partitions, rf=%d, cleanup=%s, ~%d messages retained\n%s",
		t.Name, t.PartitionCount, t.ReplicationFactor, t.CleanUpPolicy, total, b.String())
}

// FormatGroups renders consumer groups one per line.
func FormatGroups(gs []Group) string {
	var b strings.Builder
	for _, g := range gs {
		lag := "?"
		if l := g.Lag(); l >= 0 {
			lag = strconv.FormatInt(l, 10)
		}
		fmt.Fprintf(&b, "%s\tstate=%s members=%d lag=%s\n", g.GroupID, g.State, g.Members, lag)
	}
	return b.String()
}

func i64(p *int64) string {
	if p == nil {
		return "-"
	}
	return strconv.FormatInt(*p, 10)
}

// FormatGroup renders one consumer group, keeping only partitions of topics
// keep accepts (nil keeps all).
func FormatGroup(g *Group, keep func(topic string) bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "%s: state=%s members=%d\n", g.GroupID, g.State, g.Members)
	ps := append([]GroupPartition(nil), g.Partitions...)
	sort.Slice(ps, func(i, j int) bool {
		if ps[i].Topic != ps[j].Topic {
			return ps[i].Topic < ps[j].Topic
		}
		return ps[i].Partition < ps[j].Partition
	})
	hidden := 0
	var lag int64
	for _, p := range ps {
		if keep != nil && !keep(p.Topic) {
			hidden++
			continue
		}
		if p.ConsumerLag != nil {
			lag += *p.ConsumerLag
		}
		who := p.ConsumerID
		if who == "" {
			who = "(no consumer)"
		}
		fmt.Fprintf(&b, "  %s[%d] current=%s end=%s lag=%s %s\n", p.Topic, p.Partition, i64(p.CurrentOffset), i64(p.EndOffset), i64(p.ConsumerLag), who)
	}
	fmt.Fprintf(&b, "total lag shown: %d", lag)
	if hidden > 0 {
		fmt.Fprintf(&b, " (%d partitions of other topics hidden)", hidden)
	}
	b.WriteString("\n")
	return b.String()
}

// FormatMessages renders messages; values longer than maxValue are cut.
func FormatMessages(ms []Message, maxValue int) string {
	var b strings.Builder
	for _, m := range ms {
		key := "null"
		if m.Key != nil {
			key = *m.Key
		}
		val := "null"
		if b := m.Body(); b != nil {
			val = *b
		}
		if maxValue > 0 && len(val) > maxValue {
			val = val[:maxValue] + fmt.Sprintf("… (%d bytes)", len(val))
		}
		fmt.Fprintf(&b, "#%d@%d %s key=%s", m.Partition, m.Offset, m.Timestamp.Local().Format("2006-01-02 15:04:05.000"), key)
		if len(m.Headers) > 0 {
			hs := make([]string, 0, len(m.Headers))
			for k, v := range m.Headers {
				hs = append(hs, k+"="+v)
			}
			sort.Strings(hs)
			fmt.Fprintf(&b, " headers{%s}", strings.Join(hs, ", "))
		}
		fmt.Fprintf(&b, "\n  %s\n", val)
	}
	return b.String()
}

// Start sets the read mode from an offset or a time ("2h", "30m", "1d",
// RFC 3339 or local "2006-01-02T15:04"); neither: LATEST (newest first).
func (q *MessagesQuery) Start(fromOffset *int64, since string, now time.Time) error {
	since = strings.TrimSpace(since)
	switch {
	case fromOffset != nil && since != "":
		return errors.New("use either a start offset or a start time, not both")
	case fromOffset != nil:
		q.Mode, q.Offset = "FROM_OFFSET", fromOffset
	case since != "":
		t, err := ParseSince(since, now)
		if err != nil {
			return err
		}
		q.Mode, q.Since = "FROM_TIMESTAMP", &t
	default:
		q.Mode = "LATEST"
	}
	return nil
}

// ParseSince reads a duration back from now or an absolute time.
func ParseSince(s string, now time.Time) (time.Time, error) {
	s = strings.TrimSpace(s)
	if d, err := time.ParseDuration(s); err == nil {
		return now.Add(-d), nil
	}
	if n, err := strconv.Atoi(strings.TrimSuffix(s, "d")); err == nil && strings.HasSuffix(s, "d") {
		return now.AddDate(0, 0, -n), nil
	}
	if t, err := time.Parse(time.RFC3339, s); err == nil {
		return t, nil
	}
	for _, f := range []string{"2006-01-02T15:04:05", "2006-01-02T15:04", "2006-01-02 15:04", "2006-01-02"} {
		if t, err := time.ParseInLocation(f, s, time.Local); err == nil {
			return t, nil
		}
	}
	return time.Time{}, fmt.Errorf("since %q: want a duration (2h, 30m, 1d) or a time (2026-11-12T08:00)", s)
}
