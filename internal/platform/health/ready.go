package health

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"time"

	awssqs "github.com/aws/aws-sdk-go-v2/service/sqs"
	"github.com/aws/aws-sdk-go-v2/service/sqs/types"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/config"
	"github.com/NicolasPaterno/backend-challenge-go/internal/platform/httpserver"
)

// Short on purpose: a readiness probe that waits as long as a request would
// keeps a dead instance in the load balancer for exactly that long.
const probeTimeout = 2 * time.Second

type readiness struct {
	pool     *pgxpool.Pool
	sqs      *awssqs.Client
	queueURL string
	logger   *slog.Logger
}

func NewReadyRoute(pool *pgxpool.Pool, client *awssqs.Client, cfg config.Config, logger *slog.Logger) httpserver.Route {
	r := &readiness{pool: pool, sqs: client, queueURL: cfg.WagerQueueURL, logger: logger}
	return httpserver.Route{Pattern: "GET /health/ready", Handler: http.HandlerFunc(r.serve)}
}

func (r *readiness) serve(w http.ResponseWriter, req *http.Request) {
	ctx, cancel := context.WithTimeout(req.Context(), probeTimeout)
	defer cancel()

	checks := map[string]string{
		"postgres": r.check(ctx, "postgres", r.pool.Ping(ctx)),
		"sqs":      r.check(ctx, "sqs", r.queue(ctx)),
	}

	status := http.StatusOK
	body := map[string]any{"status": "ok", "checks": checks}
	for _, state := range checks {
		if state != "ok" {
			status, body["status"] = http.StatusServiceUnavailable, "unavailable"
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// The reason is logged, not returned: a public probe says which dependency is
// down, never why.
func (r *readiness) check(ctx context.Context, name string, err error) string {
	if err != nil {
		r.logger.WarnContext(ctx, "readiness check failed",
			slog.String("dependency", name), slog.Any("error", err))
		return "unavailable"
	}
	return "ok"
}

// The cheapest call that proves the queue is both reachable and ours.
func (r *readiness) queue(ctx context.Context) error {
	_, err := r.sqs.GetQueueAttributes(ctx, &awssqs.GetQueueAttributesInput{
		QueueUrl:       &r.queueURL,
		AttributeNames: []types.QueueAttributeName{types.QueueAttributeNameQueueArn},
	})
	return err
}
