//go:build integration && system

// Package system runs the guarantees against several real processes, each with
// its own connections and memory, because §8 and §13.4 ask for at least three
// and an in-process test cannot answer for them.
package system

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/jackc/pgx/v5"
	"uuid"

	"github.com/NicolasPaterno/backend-challenge-go/internal/testsupport"
)

const instances = 3

// binary is built once for the whole package, with the race detector, so the
// processes under test are checked as §13 requires and not only the harness.
var binary string

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "wagering-system")
	if err != nil {
		fmt.Fprintln(os.Stderr, "create the build directory:", err)
		os.Exit(1)
	}
	binary = filepath.Join(dir, "api")

	build := exec.Command("go", "build", "-race", "-o", binary, "./cmd/api")
	build.Dir = repoRoot()
	build.Stderr = os.Stderr
	if err := build.Run(); err != nil {
		fmt.Fprintln(os.Stderr, "build the api binary:", err)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(dir)
	os.Exit(code)
}

func repoRoot() string {
	dir, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	return filepath.Dir(filepath.Dir(dir))
}

type instance struct {
	base   string
	cmd    *exec.Cmd
	output *syncBuffer
	exited chan struct{}
}

type cluster struct {
	t           *testing.T
	databaseURL string
	sqs         *awssqs.Client
	inbound     string
	events      string
	nodes       []*instance

	internal *http.Client
	provider *http.Client
}

func startCluster(t *testing.T) *cluster {
	t.Helper()

	databaseURL := testsupport.PostgresMigrated(t)
	issuer := testsupport.KeycloakEnv(t)
	sqsClient, events := testsupport.SQSEnv(t)

	c := &cluster{
		t:           t,
		databaseURL: databaseURL,
		sqs:         sqsClient,
		inbound:     testsupport.InboundQueue(t),
		events:      events,
		internal:    testsupport.BearerClient(t, issuer, testsupport.InternalClient),
		provider:    testsupport.BearerClient(t, issuer, testsupport.ProviderAClient),
	}

	for range instances {
		c.nodes = append(c.nodes, c.start())
	}
	t.Cleanup(c.stopAll)
	// §13: whatever the scenario did, every wallet must still add up.
	t.Cleanup(c.reconcileEveryWallet)
	return c
}

func (c *cluster) start() *instance {
	c.t.Helper()

	node := &instance{
		base:   "http://" + freeAddr(c.t),
		output: &syncBuffer{},
		exited: make(chan struct{}),
	}

	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"HTTP_ADDR="+strings.TrimPrefix(node.base, "http://"),
		"DATABASE_URL="+c.databaseURL,
		"LOG_LEVEL=warn",
		// Faster than production so a worker's takeover happens inside a test
		// rather than on the one-second tick.
		"OUTBOX_POLL_INTERVAL=200ms",
		"REFERENCE_POLL_INTERVAL=200ms",
	)
	cmd.Stdout, cmd.Stderr = node.output, node.output
	if err := cmd.Start(); err != nil {
		c.t.Fatalf("start an instance: %v", err)
	}
	node.cmd = cmd
	go func() {
		_ = cmd.Wait()
		close(node.exited)
	}()

	c.await(node)
	return node
}

// await blocks until the instance reports itself ready, which is also the proof
// that it reached PostgreSQL and SQS on its own (§9).
func (c *cluster) await(node *instance) {
	c.t.Helper()

	deadline := time.Now().Add(90 * time.Second)
	for time.Now().Before(deadline) {
		resp, err := http.Get(node.base + "/health/ready")
		if err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		select {
		case <-node.exited:
			c.t.Fatalf("an instance exited before becoming ready:\n%s", node.output.String())
		case <-time.After(100 * time.Millisecond):
		}
	}
	c.t.Fatalf("an instance never became ready:\n%s", node.output.String())
}

// kill is §13.5's abrupt termination: no shutdown hook runs, so whatever the
// process was holding is released by its connections dying.
func (c *cluster) kill(node *instance) {
	c.t.Helper()

	if err := node.cmd.Process.Kill(); err != nil {
		c.t.Fatalf("kill an instance: %v", err)
	}
	<-node.exited
	c.remove(node)
}

func (c *cluster) stopAll() {
	for _, node := range c.nodes {
		_ = node.cmd.Process.Signal(os.Interrupt)
	}
	for _, node := range c.nodes {
		select {
		case <-node.exited:
		case <-time.After(30 * time.Second):
			_ = node.cmd.Process.Kill()
			<-node.exited
		}
	}
	c.nodes = nil
}

func (c *cluster) remove(dead *instance) {
	var alive []*instance
	for _, node := range c.nodes {
		if node != dead {
			alive = append(alive, node)
		}
	}
	c.nodes = alive
}

// node picks an instance round-robin, so a scenario's requests are spread over
// processes that share nothing but the database and the queue.
func (c *cluster) node(i int) *instance { return c.nodes[i%len(c.nodes)] }

func freeAddr(t *testing.T) string {
	t.Helper()

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func (c *cluster) openWallet(node *instance, playerID, balance string) string {
	c.t.Helper()

	body := fmt.Sprintf(`{"playerId":%q,"initialBalance":{"amount":%q,"currency":"BRL"}}`, playerID, balance)
	resp, err := c.internal.Post(node.base+"/wallets", "application/json", strings.NewReader(body))
	if err != nil {
		c.t.Fatalf("POST /wallets: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		c.t.Fatalf("POST /wallets status = %d, want 201", resp.StatusCode)
	}

	var opened struct {
		ID string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&opened); err != nil {
		c.t.Fatalf("decode opened wallet: %v", err)
	}
	return opened.ID
}

type operation struct {
	externalID string
	key        string
	playerID   string
	walletID   string
	amount     string
	kind       string
	round      string
	reference  string
}

func (o operation) body() string {
	if o.kind == "" {
		o.kind = "BET"
	}
	if o.round == "" {
		o.round = "round-987"
	}
	return fmt.Sprintf(`{"providerId":"provider-a","externalTransactionId":%q,"playerId":%q,`+
		`"walletId":%q,"roundId":%q,"gameId":"fortune-chimp","kind":%q,`+
		`"money":{"amount":%q,"currency":"BRL"},"referenceExternalTransactionId":%q}`,
		o.externalID, o.playerID, o.walletID, o.round, o.kind, o.amount, o.reference)
}

type outcome struct {
	Status        int
	TransactionID string
	Replay        bool
	Balance       string
	Code          string
}

func (c *cluster) submit(node *instance, o operation) outcome {
	c.t.Helper()

	key := o.key
	if key == "" {
		key = "provider-a:" + o.externalID
	}
	request, err := http.NewRequest(http.MethodPost, node.base+"/wagering/transactions", strings.NewReader(o.body()))
	if err != nil {
		c.t.Fatalf("build request: %v", err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Idempotency-Key", key)

	resp, err := c.provider.Do(request)
	if err != nil {
		c.t.Fatalf("POST /wagering/transactions: %v", err)
	}
	defer resp.Body.Close()

	var body struct {
		TransactionID    string `json:"transactionId"`
		IdempotentReplay bool   `json:"idempotentReplay"`
		Balance          struct {
			Amount string `json:"amount"`
		} `json:"balance"`
		Code string `json:"code"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		c.t.Fatalf("decode outcome (status %d): %v", resp.StatusCode, err)
	}
	return outcome{resp.StatusCode, body.TransactionID, body.IdempotentReplay, body.Balance.Amount, body.Code}
}

// enqueue is the same operation over §10's envelope, so a scenario can cross
// the two transports.
func (c *cluster) enqueue(messageID string, o operation) {
	c.t.Helper()

	key := o.key
	if key == "" {
		key = "provider-a:" + o.externalID
	}
	data := o.body()
	data = data[:len(data)-1] + fmt.Sprintf(`,"idempotencyKey":%q}`, key)
	envelope := fmt.Sprintf(`{"messageId":%q,"type":"WagerTransactionRequested",`+
		`"occurredAt":"2026-09-08T12:00:00.000Z","data":%s}`, messageID, data)

	testsupport.Send(c.t, c.sqs, c.inbound, envelope, o.walletID, messageID)
}

func (c *cluster) wallet(node *instance, walletID string) (balance string, version int64) {
	c.t.Helper()

	resp, err := c.internal.Get(node.base + "/wallets/" + walletID)
	if err != nil {
		c.t.Fatalf("GET /wallets/%s: %v", walletID, err)
	}
	defer resp.Body.Close()

	var read struct {
		Balance struct {
			Amount string `json:"amount"`
		} `json:"balance"`
		Version int64 `json:"version"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		c.t.Fatalf("decode wallet: %v", err)
	}
	return read.Balance.Amount, read.Version
}

func (c *cluster) transactionStatus(node *instance, id string) string {
	c.t.Helper()

	resp, err := c.provider.Get(node.base + "/wagering/transactions/" + id)
	if err != nil {
		c.t.Fatalf("GET /wagering/transactions/%s: %v", id, err)
	}
	defer resp.Body.Close()

	var read struct {
		Status string `json:"status"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&read); err != nil {
		c.t.Fatalf("decode transaction: %v", err)
	}
	return read.Status
}

func (c *cluster) awaitStatus(node *instance, id, want string) {
	c.t.Helper()

	deadline := time.Now().Add(60 * time.Second)
	for time.Now().Before(deadline) {
		if c.transactionStatus(node, id) == want {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	c.t.Fatalf("transaction %s never reached %s", id, want)
}

func (c *cluster) connect() *pgx.Conn {
	c.t.Helper()

	conn, err := pgx.Connect(context.Background(), c.databaseURL)
	if err != nil {
		c.t.Fatalf("connect: %v", err)
	}
	return conn
}

func (c *cluster) ledgerEntries(walletID string) int {
	c.t.Helper()

	ctx := context.Background()
	conn := c.connect()
	defer conn.Close(ctx)

	var entries int
	if err := conn.QueryRow(ctx,
		`SELECT count(*) FROM wallet_ledger_entries WHERE wallet_id = $1`, walletID).Scan(&entries); err != nil {
		c.t.Fatalf("count ledger entries: %v", err)
	}
	return entries
}

// reconcileEveryWallet is §13's closing check, run after every scenario: the
// stored balance of every wallet against the credits minus the debits of its
// ledger, read through the endpoint that rebuilds it (§9).
func (c *cluster) reconcileEveryWallet() {
	if len(c.nodes) == 0 || c.t.Failed() {
		return
	}

	ctx := context.Background()
	conn := c.connect()
	defer conn.Close(ctx)

	rows, err := conn.Query(ctx, `SELECT id FROM wallets`)
	if err != nil {
		c.t.Fatalf("list wallets: %v", err)
	}
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			c.t.Fatalf("scan wallet id: %v", err)
		}
		ids = append(ids, id)
	}
	rows.Close()

	for i, id := range ids {
		resp, err := c.internal.Post(c.node(i).base+"/wallets/"+id.String()+"/reconciliation",
			"application/json", strings.NewReader(""))
		if err != nil {
			c.t.Fatalf("POST reconciliation: %v", err)
		}
		var report struct {
			Consistent bool `json:"consistent"`
			Difference struct {
				Amount string `json:"amount"`
			} `json:"difference"`
		}
		err = json.NewDecoder(resp.Body).Decode(&report)
		resp.Body.Close()
		if err != nil {
			c.t.Fatalf("decode reconciliation: %v", err)
		}
		if !report.Consistent {
			c.t.Errorf("wallet %s diverged from its ledger by %s", id, report.Difference.Amount)
		}
	}
}
