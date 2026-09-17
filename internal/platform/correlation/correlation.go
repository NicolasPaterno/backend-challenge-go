// Package correlation carries the id that ties one operation's log lines and
// events together across processes (§12).
package correlation

import (
	"context"

	"uuid"
)

type contextKey struct{}

func NewContext(ctx context.Context, id uuid.UUID) context.Context {
	return context.WithValue(ctx, contextKey{}, id)
}

func FromContext(ctx context.Context) (uuid.UUID, bool) {
	id, ok := ctx.Value(contextKey{}).(uuid.UUID)
	return id, ok
}

// Or is for the records that must carry a correlation id whether or not the
// caller brought one: an operation resumed by a worker has no request behind
// it, and falls back to its own identity.
func Or(ctx context.Context, fallback uuid.UUID) uuid.UUID {
	if id, ok := FromContext(ctx); ok {
		return id
	}
	return fallback
}

// Ensure is the entry point of a transport: the id the caller sent, or a new
// one, so everything downstream can rely on there being one.
func Ensure(ctx context.Context, raw string) (context.Context, uuid.UUID) {
	id, err := uuid.Parse(raw)
	if err != nil {
		id = uuid.NewV7()
	}
	return NewContext(ctx, id), id
}
